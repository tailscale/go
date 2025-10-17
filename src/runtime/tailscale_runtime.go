// Copyright 2024 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

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
