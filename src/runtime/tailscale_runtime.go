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
