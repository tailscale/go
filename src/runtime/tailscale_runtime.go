// Copyright 2024 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
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
