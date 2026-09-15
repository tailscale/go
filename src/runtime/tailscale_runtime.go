// Copyright 2024 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
)

// TailscaleCurrentP returns the runtime's currently executing 'p' ID.
//
// See https://github.com/tailscale/go/issues/109.
func TailscaleCurrentP() int {
	return int(getg().m.p.ptr().id)
}

// TailscaleNumTimers returns the number of timers that are currently
// pending in the runtime across all Ps, as well as the number of "zombie"
// timers.
func TailscaleNumTimers() (total, zombies int) {
	var sum uint32
	var nzombies int32

	// Prevent allp slice changes. This is like retake.
	lock(&allpLock)
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		sum += pp.timers.len.Load()
		nzombies += pp.timers.zombies.Load()
	}
	unlock(&allpLock)

	return int(sum), int(nzombies)
}

// TailscaleStackStats describes the stack of the goroutine that
// called TailscaleReadStackStats.
//
// Goroutine stacks are allocated in power-of-two size classes starting
// at a small fixed minimum (typically 2 KiB). A stack doubles when a
// function call would overflow it, and the garbage collector halves a
// stack when it finds the goroutine using less than a quarter of it.
type TailscaleStackStats struct {
	// Size is the current size of the goroutine's stack in bytes.
	// It is always a power of two and identifies the stack's
	// current size class.
	Size uint64

	// Used is the number of bytes of stack in use by the caller at
	// the point of the TailscaleReadStackStats call. It does not
	// include the frame of TailscaleReadStackStats itself. It also
	// does not include the guard region at the bottom of the stack
	// that the runtime reserves for nosplit functions, so a goroutine
	// can trigger a stack growth with Used still noticeably less than
	// Size.
	Used uint64

	// MaxSize is the largest size in bytes that this goroutine's stack
	// has ever been. It is at least Size, and is larger than Size
	// only if the garbage collector has since shrunk the stack.
	//
	// The runtime does not track a goroutine's peak stack usage
	// directly (that would require work on every function call), but
	// because a stack only grows when the previous size overflowed,
	// the goroutine's peak usage was at least roughly MaxSize/2.
	MaxSize uint64

	// Growths is the number of times the goroutine's stack has grown.
	// It saturates at 255.
	Growths uint8

	// Shrinks is the number of times the garbage collector has shrunk
	// the goroutine's stack. It saturates at 255.
	Shrinks uint8
}

// TailscaleReadStackStats populates m with statistics about the
// calling goroutine's stack.
//
// Unlike ReadMemStats, it does not stop the world; it only reads state
// belonging to the current goroutine and is cheap to call.
//
// See https://github.com/tailscale/go/issues/184.
func TailscaleReadStackStats(m *TailscaleStackStats) {
	sp := sys.GetCallerSP()
	gp := getg()

	size := uint64(gp.stack.hi - gp.stack.lo)
	*m = TailscaleStackStats{
		Size:    size,
		Used:    uint64(gp.stack.hi - sp),
		MaxSize: size,
		Growths: gp.tsStackGrowths,
		Shrinks: gp.tsStackShrinks,
	}
	if gp.tsMaxStackOrder != 0 {
		m.MaxSize = max(size, uint64(1)<<gp.tsMaxStackOrder)
	}
}

// tailscaleNoteStackGrowth records that gp's stack is about to grow to
// newsize bytes, which must be a power of two.
// It is called from newstack.
func tailscaleNoteStackGrowth(gp *g, newsize uintptr) {
	if gp.tsStackGrowths < ^uint8(0) {
		gp.tsStackGrowths++
	}
	if order := uint8(sys.TrailingZeros64(uint64(newsize))); order > gp.tsMaxStackOrder {
		gp.tsMaxStackOrder = order
	}
}

// The goroutine stack size histogram behind the
// /tailscale/sched/goroutines-by-stack-size:bytes metric counts live
// goroutines by the power-of-two size class of their stacks. Each P
// keeps its own small array of counters, p.tsStackHist, so that
// goroutine creation and exit never touch shared memory, and
// tailscaleStackHist holds whatever is not attributed to a P. The
// counts are maintained as goroutines are created, exit, and have
// their stacks resized, so reading the metric never walks the
// goroutines.
//
// The histogram distinguishes stack sizes from
// 1<<tailscaleStackHistBaseOrder through 1<<tailscaleStackHistMaxOrder
// bytes and lumps anything larger into a final catch-all bucket.
// Nobody needs to tell a 1 GiB stack from a 512 MiB one, and keeping
// the array small lets it share a cache line with the other per-P
// fields that goroutine creation already writes.
const (
	tailscaleStackHistBaseOrder = 11 // 2 KiB, the smallest fixedStack on any platform
	tailscaleStackHistMaxOrder  = 24 // 16 MiB
	tailscaleStackHistLen       = tailscaleStackHistMaxOrder - tailscaleStackHistBaseOrder + 2

	// tailscaleStackHistSlack is how far a P's count for one size
	// class may drift from zero before it is flushed to
	// tailscaleStackHist. The per-P counts are int16, and the slack
	// keeps them far from overflowing while bounding how far off a
	// reader that catches a P mid-flush can be.
	tailscaleStackHistSlack = 1 << 10
)

// Goroutine stacks are never smaller than fixedStack, which must be at
// least 1<<tailscaleStackHistBaseOrder for the histogram indexing to
// hold. This fails to compile if it is not.
const _ uint = fixedStack - 1<<tailscaleStackHistBaseOrder

// tailscaleStackHist holds goroutine stack size counts that are not
// attributed to any P: counts flushed from Ps that drifted past
// tailscaleStackHistSlack or were destroyed, and counts recorded by
// code running without a P. It is indexed like p.tsStackHist, by
// tailscaleStackHistIndex. An entry is the number of live goroutines
// in that size class accounted for here, less the goroutines whose
// stacks were later resized or freed on some P, so an entry may be
// negative on its own. Only the sum of this array and p.tsStackHist
// across all Ps is meaningful.
var tailscaleStackHist [tailscaleStackHistLen]atomic.Int64

// tailscaleStackGrowths, tailscaleStackShrinks, and
// tailscaleStackBytesCopied back the
// /tailscale/sched/stacks/growths:events,
// /tailscale/sched/stacks/shrinks:events, and
// /tailscale/sched/stacks/copied:bytes metrics. They count every stack
// copy since program start and the bytes of stack those copies moved.
// A stack copy is rare and expensive next to an atomic add, so unlike
// the histogram these are plain global counters rather than per-P
// ones.
var (
	tailscaleStackGrowths     atomic.Uint64
	tailscaleStackShrinks     atomic.Uint64
	tailscaleStackBytesCopied atomic.Uint64
)

// tailscaleStackHistIndex returns the histogram index for a stack of
// size bytes, which must be a power of two: the stack's order relative
// to tailscaleStackHistBaseOrder, or the last index for stacks larger
// than 1<<tailscaleStackHistMaxOrder. A stack smaller than the base,
// which cannot happen, would also land in the last index rather than
// out of bounds.
func tailscaleStackHistIndex(size uintptr) int {
	i := uint(sys.TrailingZeros64(uint64(size))) - tailscaleStackHistBaseOrder
	if i >= tailscaleStackHistLen {
		i = tailscaleStackHistLen - 1
	}
	return int(i)
}

// tailscaleStackHistAdd records that delta more live goroutines (or
// fewer, if delta is negative) have a stack of size bytes.
//
// pp is the caller's P, or nil to use the global counter instead. If
// pp is non-nil, the caller must own it and must not be preemptible;
// all callers satisfy this by running on the system stack or with an
// M held.
func tailscaleStackHistAdd(pp *p, size uintptr, delta int16) {
	i := tailscaleStackHistIndex(size)
	if pp == nil {
		tailscaleStackHist[i].Add(int64(delta))
		return
	}
	n := pp.tsStackHist[i] + delta
	if n >= tailscaleStackHistSlack || n <= -tailscaleStackHistSlack {
		tailscaleStackHist[i].Add(int64(n))
		n = 0
	}
	pp.tsStackHist[i] = n
}

// tailscaleNoteStackCopy records that a live goroutine's stack of
// oldsize bytes, of which used bytes are in use, is being replaced by
// one of newsize bytes. It moves the goroutine between size classes in
// the histogram and updates the cumulative growth, shrink, and copied
// byte counters. It is called from copystack.
func tailscaleNoteStackCopy(pp *p, oldsize, newsize, used uintptr) {
	tailscaleStackHistAdd(pp, oldsize, -1)
	tailscaleStackHistAdd(pp, newsize, 1)
	if newsize > oldsize {
		tailscaleStackGrowths.Add(1)
	} else {
		tailscaleStackShrinks.Add(1)
	}
	tailscaleStackBytesCopied.Add(int64(used))
}

// tailscaleStackHistFlush moves pp's stack size counts into
// tailscaleStackHist so that they survive pp being destroyed.
//
// The world must be stopped.
func tailscaleStackHistFlush(pp *p) {
	assertWorldStopped()
	for i := range pp.tsStackHist {
		if n := pp.tsStackHist[i]; n != 0 {
			tailscaleStackHist[i].Add(int64(n))
			pp.tsStackHist[i] = 0
		}
	}
}

// tailscaleStackHistFirst returns the index of the size class of the
// smallest stack a goroutine can have, which is the first bucket the
// metric reports. On most platforms fixedStack is 2 KiB and this is
// 0, but a larger fixedStack, as with the race detector, leaves the
// low entries permanently unused.
func tailscaleStackHistFirst() int {
	return tailscaleStackHistIndex(fixedStack)
}

// tailscaleStackHistBuckets returns the bucket boundaries of the
// /tailscale/sched/goroutines-by-stack-size:bytes histogram: every
// power of two from fixedStack through
// 1<<(tailscaleStackHistMaxOrder+1), then +Inf. Each bucket but the
// last therefore counts exactly the goroutines whose stack is the
// bucket's lower bound, and the last counts every goroutine with a
// larger stack.
func tailscaleStackHistBuckets() []float64 {
	minOrder := sys.TrailingZeros64(uint64(fixedStack))
	buckets := make([]float64, 0, tailscaleStackHistMaxOrder+3-minOrder)
	for o := minOrder; o <= tailscaleStackHistMaxOrder+1; o++ {
		buckets = append(buckets, float64(uint64(1)<<o))
	}
	return append(buckets, float64Inf())
}

// tailscaleStackHistRead sums the per-P and global stack size counts
// into counts, whose length must match the buckets returned by
// tailscaleStackHistBuckets. It sets counts[i] to the number of live
// goroutines whose stack size falls in bucket i.
//
// It holds sched.lock so that the set of Ps cannot change underneath
// it, but it reads the per-P counts without synchronizing against the
// Ps updating them, as gcount does. The result is therefore
// approximate in the same way /sched/goroutines:goroutines is: a
// goroutine that is being created, exiting, or resizing its stack
// concurrently may be counted in two buckets or in none, and a read
// that catches a P flushing a count to tailscaleStackHist can be off
// by up to tailscaleStackHistSlack in that bucket.
func tailscaleStackHistRead(counts []uint64) {
	first := tailscaleStackHistFirst()
	lock(&sched.lock)
	for i := range counts {
		j := first + i
		n := tailscaleStackHist[j].Load()
		for _, pp := range allp {
			if pp == nil {
				break
			}
			n += int64(pp.tsStackHist[j])
		}
		// The sum can only go negative transiently, when the reads
		// above straddled a goroutine's exit or stack resize.
		counts[i] = uint64(max(n, 0))
	}
	unlock(&sched.lock)
}
