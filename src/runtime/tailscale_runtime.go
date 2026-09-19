// Copyright 2024 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	_ "unsafe" // for go:linkname
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
	c := readTailscaleTimerCounts()
	return int(c.tracked), int(c.zombies)
}

// tailscaleTimerCounts is a snapshot of the per-P timer heap counters
// summed over all Ps. Each field is read separately and without stopping
// the world, so the fields are individually accurate but may not be
// mutually consistent.
type tailscaleTimerCounts struct {
	tracked     int64 // timers in the heaps, including zombies
	zombies     int64 // heaped timers that have been stopped
	zombieChans int64 // zombies that are channel timers
}

// zombieFuncs returns the number of zombies that are not channel
// timers: AfterFunc timers and runtime-internal ones such as netpoll
// deadlines. Those are the zombies that pin a closure until the owning P
// removes them from its heap.
func (c tailscaleTimerCounts) zombieFuncs() int64 {
	return max(c.zombies-c.zombieChans, 0)
}

// readTailscaleTimerCounts sums the timer heap counters over all Ps.
// Timers in synctest bubbles live in the bubble's heap and are not
// counted.
func readTailscaleTimerCounts() (c tailscaleTimerCounts) {
	// Prevent allp slice changes. This is like retake.
	lock(&allpLock)
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		c.tracked += int64(pp.timers.len.Load())
		c.zombies += int64(pp.timers.zombies.Load())
		c.zombieChans += int64(pp.timers.zombieChans.Load())
	}
	unlock(&allpLock)
	// The sums can only go negative transiently, when a read straddled
	// a stop or a heap cleanup on another P.
	c.zombies = max(c.zombies, 0)
	c.zombieChans = max(c.zombieChans, 0)
	return c
}

// zombieAdd adjusts ts.zombies by delta on behalf of the timer t, which
// is being marked as (delta > 0) or unmarked as (delta < 0) a zombie.
// It also keeps the Tailscale-specific per-kind count in ts.zombieChans,
// and when a zombie is unmarked it closes out the func zombie lifetime
// accounting described at tailscaleFuncZombieBuckets. The caller must
// hold t's lock, as for any other change to t.state.
func (ts *timers) zombieAdd(t *timer, delta int32) {
	ts.zombies.Add(delta)
	if t.isChan {
		ts.zombieChans.Add(delta)
	}
	if delta < 0 {
		ts.tailscaleFuncZombieRecord(t)
	}
}

// Func zombie lifetime accounting.
//
// The /tailscale/sched/timers/zombies/func:timers gauge says how many
// stopped func timers are sitting in the heaps holding their functions,
// but not for how long. A large steady value is consistent both with a
// high rate of zombies that each last microseconds and with a modest
// rate of zombies that each last through many garbage collections, and
// only the latter costs anything. The metrics under
// /tailscale/sched/timers/zombies/func/ tell the two apart. created and
// removed are cumulative counts, so the gauge divided by the creation
// rate is the mean time a func zombie holds its function, and lifetime
// is a histogram of that time measured in GC cycles, which is the unit
// that matters: a zombie removed before the next cycle began cost the
// collector nothing, while one that lasted five cycles had its function
// and everything it references marked live five times.
//
// Recording the histogram needs to know, at removal, when the zombie was
// made. Rather than grow every timer by a field, the GC cycle count at
// the time of the Stop is stashed in t.when, as -(cycles+1). A stopped
// func timer otherwise has t.when == 0 until a Reset, and every read of
// t.when is either a "t.when > 0" pending test, which treats the stash
// like zero, or is unreachable for a heaped zombie, so the stash is
// invisible to upstream code. The stash is cleared when it is read, so a
// timer that is not a heaped zombie never has a negative t.when. Channel
// timers are left alone: unblockTimerChan makes zombies of them without
// clearing t.when, they pin only their channel, and they are not what
// the metric is for. Neither are timers made zombies by their own run in
// unlockAndRun, which are removed from the heap in the same call, nor
// timers released with TailscaleRelease, which hold no function.
//
// The counts live in the owning P's timers struct, next to the zombie
// counters they refine, and are flushed to the globals below when a P is
// destroyed.

// tailscaleFuncZombieBuckets is the number of buckets in the
// /tailscale/sched/timers/zombies/func/lifetime:gc-cycles histogram.
// Bucket 0 holds zombies removed before another GC cycle began, and
// bucket i > 0 holds those that lasted from 1<<(i-1) up to but not
// including 1<<i cycles, except that the last bucket has no upper bound.
const tailscaleFuncZombieBuckets = 7

// tailscaleFuncZombiesCreated and tailscaleFuncZombieCycles hold the
// counts flushed from destroyed Ps. Only their sums with the per-P counts
// are meaningful.
var (
	tailscaleFuncZombiesCreated atomic.Uint64
	tailscaleFuncZombieCycles   [tailscaleFuncZombieBuckets]atomic.Uint64
)

// tailscaleFuncZombieBucket returns the histogram bucket for a zombie
// that lasted through the start of cycles GC cycles.
func tailscaleFuncZombieBucket(cycles uint32) int {
	return min(sys.Len64(uint64(cycles)), tailscaleFuncZombieBuckets-1)
}

// tailscaleFuncZombieCyclesBuckets returns the bucket boundaries of the
// lifetime histogram: 0, 1, then successive powers of two, then +Inf.
func tailscaleFuncZombieCyclesBuckets() []float64 {
	buckets := make([]float64, 0, tailscaleFuncZombieBuckets+1)
	buckets = append(buckets, 0)
	for i := 1; i < tailscaleFuncZombieBuckets; i++ {
		buckets = append(buckets, float64(uint64(1)<<(i-1)))
	}
	return append(buckets, float64Inf())
}

// tailscaleFuncZombieStamp records that t, a func timer that the caller
// has just made a zombie by stopping it, now holds its function for no
// reason. It stashes the current GC cycle count in t.when, which the
// caller must already have cleared. The caller must hold t's lock.
func (t *timer) tailscaleFuncZombieStamp() {
	if t.isChan || t.when != 0 {
		throw("bad func zombie stamp")
	}
	t.when = -int64(work.cycles.Load()) - 1
	t.ts.tsFuncZombiesCreated.Add(1)
}

// tailscaleFuncZombieRecord closes out the accounting for t if it is a
// stamped func zombie: it counts t in ts's lifetime histogram by the
// number of GC cycles begun since the stamp and clears the stamp. It
// does nothing for a timer without a stamp. The caller must hold t's
// lock or have the world stopped.
func (ts *timers) tailscaleFuncZombieRecord(t *timer) {
	if t.when >= 0 {
		return
	}
	// The subtraction wraps correctly if the cycle count did.
	cycles := work.cycles.Load() - uint32(-(t.when + 1))
	t.when = 0
	ts.tsFuncZombieCycles[tailscaleFuncZombieBucket(cycles)].Add(1)
}

// tailscaleFuncZombieUnstamp is tailscaleFuncZombieRecord against t's
// own heap, for callers that are about to overwrite t.when.
func (t *timer) tailscaleFuncZombieUnstamp() {
	if t.when < 0 {
		t.ts.tailscaleFuncZombieRecord(t)
	}
}

// tailscaleFuncZombieFlush moves ts's cumulative func zombie counts into
// the globals so that they survive the P that owns ts being destroyed.
//
// The world must be stopped.
func tailscaleFuncZombieFlush(ts *timers) {
	assertWorldStopped()
	tailscaleFuncZombiesCreated.Add(int64(ts.tsFuncZombiesCreated.Swap(0)))
	for i := range ts.tsFuncZombieCycles {
		tailscaleFuncZombieCycles[i].Add(int64(ts.tsFuncZombieCycles[i].Swap(0)))
	}
}

// tailscaleFuncZombieRead sums the per-P and global func zombie counts.
// It returns the number of func zombies created and, if counts is
// non-nil, fills it with the lifetime histogram, whose length must be
// tailscaleFuncZombieBuckets. The entries of counts sum to the number of
// func zombies removed. Like readTailscaleTimerCounts, it holds allpLock
// so that the set of Ps cannot change under it, but reads the counters
// without stopping the world, so the results are individually accurate
// rather than mutually consistent.
func tailscaleFuncZombieRead(counts []uint64) (created uint64) {
	lock(&allpLock)
	created = tailscaleFuncZombiesCreated.Load()
	for i := range counts {
		counts[i] = tailscaleFuncZombieCycles[i].Load()
	}
	for _, pp := range allp {
		if pp == nil {
			continue
		}
		created += pp.timers.tsFuncZombiesCreated.Load()
		for i := range counts {
			counts[i] += pp.timers.tsFuncZombieCycles[i].Load()
		}
	}
	unlock(&allpLock)
	return created
}

// releaseTimer implements time.(*Timer).TailscaleRelease.
// It reports whether the timer was stopped before it was run, like
// stopTimer.
//
//go:linkname releaseTimer time.releaseTimer
func releaseTimer(t *timeTimer) bool {
	if t.isFake && getg().bubble == nil {
		fatal("release of synctest timer from outside bubble")
	}
	return t.release()
}

// timerReleased is a timer state bit set by (*timer).release and never
// cleared. resetTimer panics when it is set. It is a Tailscale addition
// and takes the top bit of the state byte to stay clear of the bits
// upstream defines in time.go.
const timerReleased uint8 = 1 << 7

// release stops t and, for a func timer, drops t's reference to the
// function it would have called, so that the function and everything it
// captures can be collected even while t sits in a heap as a zombie.
// The caller promises never to reset t again; resetTimer enforces that
// with a panic once timerReleased is set. Reports whether the timer was
// stopped before it was run.
//
// A channel timer is only stopped and marked released. Its arg is its
// channel, which the runtime keeps finding through t.hchan as long as
// user code can still receive from the channel, and a zombie channel
// timer pins only the small timer allocation and its channel anyway.
func (t *timer) release() bool {
	if t.isChan {
		t.lock()
		t.trace("release")
		t.state |= timerReleased
		t.unlock()
		return t.stop()
	}

	// This mirrors the non-channel half of t.stop, so that the stop and
	// the clearing of t.arg happen under one hold of the lock and no
	// concurrent run can observe a live timer with a cleared arg.
	t.lock()
	t.trace("release")
	if t.state&timerHeaped != 0 {
		t.state |= timerModified
		if t.state&timerZombie == 0 {
			t.state |= timerZombie
			t.ts.zombieAdd(t, 1)
		}
	}
	t.state |= timerReleased
	pending := t.when > 0
	// If an earlier Stop left t a zombie holding its function, that
	// ends here. A timer that release itself just made a zombie is not
	// stamped, since it holds no function from here on.
	t.tailscaleFuncZombieUnstamp()
	t.when = 0
	t.f = tailscaleReleasedTimerFunc
	t.arg = nil
	t.unlock()
	return pending
}

// tailscaleReleasedTimerFunc is installed as t.f by t.release. A released
// timer is a zombie with t.when == 0 and can only run again if user code
// races a Reset against the release, which resetTimer otherwise rejects,
// so reaching this is a bug.
func tailscaleReleasedTimerFunc(arg any, seq uintptr, delay int64) {
	throw("released timer ran")
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
