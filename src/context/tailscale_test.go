// Copyright 2026 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package context_test

import (
	. "context"
	"os"
	"runtime"
	"testing"
	"time"
	"weak"
)

type tailscaleKey struct{}

// tailscaleCanceledTimeoutCtx builds a context chain holding a fresh
// allocation as a value, derives a timeout context from it, cancels
// that, and returns only a weak pointer to the allocation.
//
//go:noinline
func tailscaleCanceledTimeoutCtx() weak.Pointer[[64]byte] {
	p := new([64]byte)
	parent := WithValue(Background(), tailscaleKey{}, p)
	ctx, cancel := WithTimeout(parent, time.Hour)
	cancel()
	if ctx.Err() != Canceled {
		panic("context not canceled")
	}
	return weak.Make(p)
}

// TestTailscaleTimeoutCancelReleasesTimer checks that canceling a
// timeout context before its deadline does not leave the context chain
// pinned by the stopped timer sitting in the runtime's timer heap.
// The behavior is opt-in, and the package reads the environment variable
// at init, so the test needs TS_RELEASE_CONTEXT_TIMER=1 set on the test
// process.
func TestTailscaleTimeoutCancelReleasesTimer(t *testing.T) {
	if os.Getenv("TS_RELEASE_CONTEXT_TIMER") != "1" {
		t.Skip("TS_RELEASE_CONTEXT_TIMER=1 not set; timer release is opt-in")
	}
	// Keep live long timers around so that the canceled context's timer
	// is well under a quarter of the heap. Otherwise the runtime sweeps
	// the zombie on its next timer check, and the test would pass with a
	// plain Stop too, for the wrong reason.
	var live []*time.Timer
	for range 8 {
		live = append(live, time.AfterFunc(time.Hour, func() {}))
	}
	defer func() {
		for _, tm := range live {
			tm.Stop()
		}
	}()

	wp := tailscaleCanceledTimeoutCtx()
	runtime.GC()
	if wp.Value() != nil {
		t.Error("value on a canceled timeout context's parent is still reachable; the context's timer was not released")
	}
}
