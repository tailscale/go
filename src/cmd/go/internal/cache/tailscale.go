// Copyright 2026 Tailscale Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import "runtime/debug"

func init() {
	if rev, ok := debug.TailscaleToolchainGitRev(); ok {
		hashSalt = append(hashSalt, ' ')
		hashSalt = append(hashSalt, rev...)
	}
}
