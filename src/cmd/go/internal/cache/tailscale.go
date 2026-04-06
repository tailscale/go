// Copyright 2026 Tailscale Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import "runtime/debug"

func init() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range bi.Settings {
		if s.Key == "tailscale.toolchain.rev" && s.Value != "" {
			hashSalt = append(hashSalt, ' ')
			hashSalt = append(hashSalt, s.Value...)
			return
		}
	}
}
