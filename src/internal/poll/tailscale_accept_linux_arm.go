// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file is a Tailscale addition. See tailscale/go#195 and
// tailscale/tailscale#18268.
//
// The accept4 system call was added to Linux in 2.6.28, but on ARM it
// was not wired up until 2.6.36. Upstream Go carried a fallback to
// plain accept for linux/arm (golang/go#57333) until Go 1.24 raised
// the minimum kernel to 3.2 and removed it (CL 588216). Old 32-bit ARM
// devices such as Synology DS112/DS411 (88f6281/88f6282, DSM 6.2,
// Linux 2.6.32) still run Tailscale, and without the fallback every
// net.Listener.Accept fails with "accept4: function not implemented".
//
// To keep this patch trivially rebasable, it lives entirely in this
// file and touches no upstream code: at init it wraps Accept4Func
// (the hook that sock_cloexec.go's accept already calls) with a
// version that falls back to accept(2) when accept4 is missing.

//go:build linux && arm

package poll

import (
	"sync/atomic"
	"syscall"
)

func init() {
	tsAccept4 = Accept4Func
	Accept4Func = tsAccept4WithFallback
}

// tsAccept4 is the real accept4 implementation that
// tsAccept4WithFallback tries first. Tests can replace it to simulate
// an old kernel.
var tsAccept4 func(int, int) (int, syscall.Sockaddr, error)

// tsNoAccept4 is set once accept4 has returned ENOSYS, so later calls
// skip straight to accept.
var tsNoAccept4 atomic.Bool

// tsAccept4WithFallback is like syscall.Accept4 but emulates it with
// accept, fcntl, and getpeername on kernels that lack accept4.
func tsAccept4WithFallback(s, flags int) (int, syscall.Sockaddr, error) {
	if !tsNoAccept4.Load() {
		ns, sa, err := tsAccept4(s, flags)
		switch err {
		default:
			return ns, sa, err
		case syscall.ENOSYS:
			tsNoAccept4.Store(true)
		case syscall.EINVAL, syscall.EACCES, syscall.EFAULT:
			// Some kernels report a missing accept4 with these
			// instead of ENOSYS (see the Go 1.17 accept code).
			// They are also legitimate accept4 errors, so don't
			// remember them; accept will return the same error
			// if it's real.
		}
	}

	// See ../syscall/exec_unix.go for description of ForkLock.
	// As in sys_cloexec.go, we don't hold ForkLock across accept,
	// because the listener may have been put back into blocking mode
	// via the File method, and blocking while holding ForkLock would
	// block all forks.
	r, _, e := syscall.Syscall(syscall.SYS_ACCEPT, uintptr(s), 0, 0)
	if e != 0 {
		return -1, nil, e
	}
	ns := int(r)
	if flags&syscall.SOCK_CLOEXEC != 0 {
		syscall.CloseOnExec(ns)
	}
	if flags&syscall.SOCK_NONBLOCK != 0 {
		if err := syscall.SetNonblock(ns, true); err != nil {
			CloseFunc(ns)
			return -1, nil, err
		}
	}
	// We passed no address buffer to accept, because converting a
	// RawSockaddrAny to a Sockaddr needs syscall internals. Ask
	// getpeername instead. It returns the same address accept would
	// have, including an unnamed AF_UNIX address for unbound unix
	// socket peers.
	sa, err := syscall.Getpeername(ns)
	if err != nil {
		// The peer went away (ENOTCONN) between accept and
		// getpeername. Report it like a connection that was aborted
		// before accept, which FD.Accept retries.
		CloseFunc(ns)
		return -1, nil, syscall.ECONNABORTED
	}
	return ns, sa, nil
}
