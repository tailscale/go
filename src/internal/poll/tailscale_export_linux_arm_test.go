// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package poll

import "syscall"

// SetTailscaleAccept4ForTest replaces the accept4 implementation that
// the linux/arm accept fallback tries first, and resets its memory of
// whether accept4 is missing. It returns a func that restores the
// previous state.
func SetTailscaleAccept4ForTest(f func(int, int) (int, syscall.Sockaddr, error)) (restore func()) {
	old := tsAccept4
	tsAccept4 = f
	tsNoAccept4.Store(false)
	return func() {
		tsAccept4 = old
		tsNoAccept4.Store(false)
	}
}

// TailscaleNoAccept4 reports whether the linux/arm accept fallback has
// concluded that accept4 is missing.
func TailscaleNoAccept4() bool { return tsNoAccept4.Load() }
