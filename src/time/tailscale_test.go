// Copyright 2026 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package time_test

import (
	"fmt"
	"runtime"
	"testing"
	"testing/synctest"
	. "time"
	"weak"
)

// tailscaleMustPanic calls f and checks that it panics with want.
func tailscaleMustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("no panic; want %q", want)
		} else if got := fmt.Sprint(r); got != want {
			t.Errorf("panic = %q; want %q", got, want)
		}
	}()
	f()
}

func TestTailscaleRelease(t *testing.T) {
	tm := AfterFunc(Hour, func() { t.Error("released timer fired") })
	if !tm.TailscaleRelease() {
		t.Error("TailscaleRelease = false; want true for a pending timer")
	}
	if tm.TailscaleRelease() {
		t.Error("second TailscaleRelease = true; want false")
	}
	if tm.Stop() {
		t.Error("Stop after TailscaleRelease = true; want false")
	}
	tailscaleMustPanic(t, "time: Reset called on released Timer", func() { tm.Reset(Hour) })
}

func TestTailscaleReleaseChanTimer(t *testing.T) {
	tm := NewTimer(Hour)
	if !tm.TailscaleRelease() {
		t.Error("TailscaleRelease = false; want true for a pending timer")
	}
	// The channel must still be usable, since user code may hold it.
	select {
	case <-tm.C:
		t.Error("received from released timer")
	default:
	}
	if tm.Stop() {
		t.Error("Stop after TailscaleRelease = true; want false")
	}
	tailscaleMustPanic(t, "time: Reset called on released Timer", func() { tm.Reset(Hour) })
}

func TestTailscaleReleaseUninitialized(t *testing.T) {
	var tm Timer
	tailscaleMustPanic(t, "time: TailscaleRelease called on uninitialized Timer", func() { tm.TailscaleRelease() })
}

func TestTailscaleReleaseFired(t *testing.T) {
	fired := make(chan struct{})
	tm := AfterFunc(0, func() { close(fired) })
	<-fired
	if tm.TailscaleRelease() {
		t.Error("TailscaleRelease on a fired timer = true; want false")
	}
	tailscaleMustPanic(t, "time: Reset called on released Timer", func() { tm.Reset(Hour) })
}

// TestTailscaleReleaseFromFunc releases a timer from inside its own
// function, as code that owns a timer might do on its way out.
func TestTailscaleReleaseFromFunc(t *testing.T) {
	var tm *Timer
	ready := make(chan struct{})
	result := make(chan bool)
	tm = AfterFunc(Millisecond, func() {
		<-ready
		result <- tm.TailscaleRelease()
	})
	close(ready)
	if <-result {
		t.Error("TailscaleRelease from the timer's own func = true; want false")
	}
}

// tailscaleTimerCapturing returns a pending AfterFunc timer whose
// function captures a fresh allocation, along with a weak pointer to that
// allocation. Nothing else refers to the allocation once this returns.
//
//go:noinline
func tailscaleTimerCapturing() (*Timer, weak.Pointer[[64]byte]) {
	p := new([64]byte)
	tm := AfterFunc(Hour, func() { p[0]++ })
	return tm, weak.Make(p)
}

// TestTailscaleReleaseDropsFunc checks the point of TailscaleRelease:
// that a released timer no longer keeps its function's captures alive,
// while a merely stopped one does.
func TestTailscaleReleaseDropsFunc(t *testing.T) {
	stopped, wpStopped := tailscaleTimerCapturing()
	stopped.Stop()
	released, wpReleased := tailscaleTimerCapturing()
	released.TailscaleRelease()

	runtime.GC()

	if wpStopped.Value() == nil {
		t.Error("allocation captured by a stopped timer's func was collected; want it kept alive by the timer")
	}
	if wpReleased.Value() != nil {
		t.Error("allocation captured by a released timer's func is still reachable")
	}
	// Both timers are kept alive here so that the comparison is about
	// what the timers reference, not about whether the timers themselves
	// were collected.
	runtime.KeepAlive(stopped)
	runtime.KeepAlive(released)
}

func TestTailscaleReleaseSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := AfterFunc(Hour, func() { t.Error("released timer fired") })
		if !tm.TailscaleRelease() {
			t.Error("TailscaleRelease = false; want true for a pending timer")
		}
		tailscaleMustPanic(t, "time: Reset called on released Timer", func() { tm.Reset(Hour) })
	})
}
