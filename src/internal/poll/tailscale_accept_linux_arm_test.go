// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package poll_test

import (
	"internal/poll"
	"net"
	"path/filepath"
	"syscall"
	"testing"
)

// TestTailscaleAcceptFallback simulates a pre-2.6.36 ARM kernel, where
// accept4 returns ENOSYS, and checks that net listeners still accept
// connections with the right fd flags and peer address.
// See tailscale/go#195.
func TestTailscaleAcceptFallback(t *testing.T) {
	var accept4Calls int
	restore := poll.SetTailscaleAccept4ForTest(func(int, int) (int, syscall.Sockaddr, error) {
		accept4Calls++
		return -1, nil, syscall.ENOSYS
	})
	defer restore()

	unixPath := filepath.Join(t.TempDir(), "sock")
	for _, tt := range []struct{ network, addr string }{
		{"tcp4", "127.0.0.1:0"},
		{"unix", unixPath},
		{"unix", "@tsgo-accept-fallback-test"},
	} {
		t.Run(tt.network+"_"+tt.addr, func(t *testing.T) {
			ln, err := net.Listen(tt.network, tt.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			// Dial twice to cover both the first call (which tries
			// accept4) and later calls (which skip it).
			for i := range 2 {
				c, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()

				sc, err := ln.Accept()
				if err != nil {
					t.Fatalf("Accept #%d: %v", i, err)
				}
				defer sc.Close()

				if tt.network == "tcp4" {
					if got, want := sc.RemoteAddr().String(), c.LocalAddr().String(); got != want {
						t.Errorf("RemoteAddr = %q; want %q", got, want)
					}
				}
				checkAcceptedFlags(t, sc)

				// Make sure the conn works through the poller.
				if _, err := c.Write([]byte("hi")); err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, 2)
				if _, err := sc.Read(buf); err != nil || string(buf) != "hi" {
					t.Fatalf("Read = %q, %v; want \"hi\"", buf, err)
				}
			}
		})
	}
	if accept4Calls != 1 {
		t.Errorf("accept4 called %d times; want 1 (later calls should skip it)", accept4Calls)
	}
	if !poll.TailscaleNoAccept4() {
		t.Errorf("fallback did not remember that accept4 is missing")
	}
}

func checkAcceptedFlags(t *testing.T, c net.Conn) {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fl, fdfl uintptr
	var errno syscall.Errno
	rc.Control(func(fd uintptr) {
		fl, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		if errno == 0 {
			fdfl, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
		}
	})
	if errno != 0 {
		t.Fatalf("fcntl: %v", errno)
	}
	if fl&syscall.O_NONBLOCK == 0 {
		t.Errorf("accepted fd is not O_NONBLOCK")
	}
	if fdfl&syscall.FD_CLOEXEC == 0 {
		t.Errorf("accepted fd is not FD_CLOEXEC")
	}
}
