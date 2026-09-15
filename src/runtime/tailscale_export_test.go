// Copyright 2026 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// TailscaleStackHistSlow stops the world and returns the goroutine
// stack size histogram computed two ways: want, by walking every
// goroutine, and got, from the incrementally maintained counts via the
// same code path the /tailscale/sched/goroutines-by-stack-size:bytes
// metric uses. Both are indexed like the metric's buckets. Nothing can
// change while the world is stopped, so the two must match exactly.
func TailscaleStackHistSlow() (want, got []uint64) {
	stw := stopTheWorld(stwForTestReadMetricsSlow)
	first := tailscaleStackHistFirst()
	want = make([]uint64, tailscaleStackHistLen-first)
	got = make([]uint64, tailscaleStackHistLen-first)
	forEachG(func(gp *g) {
		// Dead goroutines waiting on a free list still own a stack,
		// but they are not live and are not counted. Goroutines that
		// belong to extra Ms are _Gdeadextra rather than _Gdead
		// between cgo callbacks, and those are counted.
		if readgstatus(gp)&^_Gscan == _Gdead {
			return
		}
		want[tailscaleStackHistIndex(gp.stack.hi-gp.stack.lo)-first]++
	})
	tailscaleStackHistRead(got)
	startTheWorld(stw)
	return want, got
}

// TailscaleStackHistLayout reports where p.tsStackHist and
// p.goroutinesCreated live within p, so that a test can check that
// they share a cache line.
func TailscaleStackHistLayout() (histOff, histSize, goroutinesCreatedOff uintptr) {
	return unsafe.Offsetof(p{}.tsStackHist), unsafe.Sizeof(p{}.tsStackHist), unsafe.Offsetof(p{}.goroutinesCreated)
}
