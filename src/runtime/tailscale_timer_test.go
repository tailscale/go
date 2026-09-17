// Copyright 2026 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"runtime/metrics"
	"testing"
	"time"
)

// tsTimerCounts holds one reading of the Tailscale timer metrics.
type tsTimerCounts struct {
	tracked, zombies, chans, funcs int64
}

func readTSTimerCounts(t *testing.T) tsTimerCounts {
	t.Helper()
	s := []metrics.Sample{
		{Name: "/tailscale/sched/timers/tracked:timers"},
		{Name: "/tailscale/sched/timers/zombies:timers"},
		{Name: "/tailscale/sched/timers/zombies/chan:timers"},
		{Name: "/tailscale/sched/timers/zombies/func:timers"},
	}
	metrics.Read(s)
	var c tsTimerCounts
	for i, dst := range []*int64{&c.tracked, &c.zombies, &c.chans, &c.funcs} {
		if kind := s[i].Value.Kind(); kind != metrics.KindUint64 {
			t.Fatalf("%s: kind = %v; want KindUint64", s[i].Name, kind)
		}
		*dst = int64(s[i].Value.Uint64())
	}
	return c
}

// TestTailscaleTimerMetricsFunc checks that stopped AfterFunc timers
// show up as func zombies.
func TestTailscaleTimerMetricsFunc(t *testing.T) {
	// The owning P sweeps zombies out of its heap when they exceed a
	// quarter of it, so keep four live timers per stopped one. Creating
	// them interleaved spreads the zombies through the heap so that a
	// stray timer add on the same P, which trims one zombie off the
	// heap's tail, can cost at most one of them.
	const n = 20
	var live, stopped []*time.Timer
	defer func() {
		for _, tm := range live {
			tm.Stop()
		}
	}()
	for range n {
		for range 4 {
			live = append(live, time.AfterFunc(time.Hour, func() {}))
		}
		stopped = append(stopped, time.AfterFunc(time.Hour, func() {}))
	}

	before := readTSTimerCounts(t)
	for _, tm := range stopped {
		if !tm.Stop() {
			t.Fatal("Stop = false; want true")
		}
	}
	after := readTSTimerCounts(t)
	t.Logf("before: %+v", before)
	t.Logf("after:  %+v", after)

	if got := after.funcs - before.funcs; got < n-1 {
		t.Errorf("func zombies grew by %d after stopping %d AfterFunc timers; want at least %d", got, n, n-1)
	}
	if got := after.zombies - before.zombies; got < n-1 {
		t.Errorf("zombies grew by %d after stopping %d AfterFunc timers; want at least %d", got, n, n-1)
	}
	if after.tracked < 5*n {
		t.Errorf("tracked = %d; want at least the %d timers this test created", after.tracked, 5*n)
	}
	if after.zombies < after.chans || after.zombies < after.funcs {
		t.Errorf("zombies = %d, less than chan (%d) or func (%d) zombies", after.zombies, after.chans, after.funcs)
	}

	total, zombies := runtime.TailscaleNumTimers()
	if int64(total) < 5*n || int64(zombies) < n-1 {
		t.Errorf("TailscaleNumTimers = %d, %d; want at least %d, %d", total, zombies, 5*n, n-1)
	}
}

// TestTailscaleTimerMetricsChan checks that a channel timer whose
// waiting goroutine is woken by something else shows up as a chan
// zombie. A channel timer is only in a heap while a goroutine is blocked
// on its channel, and there is no direct way to observe that the
// goroutine has parked, so the test polls the tracked count and retries
// if it guessed wrong.
func TestTailscaleTimerMetricsChan(t *testing.T) {
	// Live padding timers keep the one zombie this test creates per
	// attempt under a quarter of the heap, so the owning P does not sweep
	// it before the test reads the metrics. Adding a batch after each
	// attempt also buries that attempt's zombie under live timers, so the
	// next attempt's timer add does not trim it off the heap's tail and
	// cancel out the count the test is looking for.
	var pad []*time.Timer
	defer func() {
		for _, tm := range pad {
			tm.Stop()
		}
	}()
	addPad := func() {
		for range 8 {
			pad = append(pad, time.AfterFunc(time.Hour, func() {}))
		}
	}
	addPad()

	const attempts = 5
	for attempt := 1; attempt <= attempts; attempt++ {
		before := readTSTimerCounts(t)
		tm := time.NewTimer(time.Hour)
		done := make(chan struct{})
		exited := make(chan struct{})
		go func() {
			defer close(exited)
			select {
			case <-tm.C:
			case <-done:
			}
		}()

		deadline := time.Now().Add(5 * time.Second)
		parked := false
		for time.Now().Before(deadline) {
			if readTSTimerCounts(t).tracked > before.tracked {
				parked = true
				break
			}
			runtime.Gosched()
		}
		if !parked {
			t.Logf("attempt %d: timer never appeared in a heap", attempt)
			close(done)
			<-exited
			tm.Stop()
			continue
		}

		// Waking the goroutine through done, rather than by the timer
		// firing, leaves the timer in the heap marked as a zombie.
		close(done)
		<-exited
		after := readTSTimerCounts(t)
		tm.Stop()
		t.Logf("attempt %d: before: %+v", attempt, before)
		t.Logf("attempt %d: after:  %+v", attempt, after)
		if after.chans > before.chans {
			return
		}
		t.Logf("attempt %d: chan zombies did not grow; retrying", attempt)
	}
	t.Fatalf("no chan zombie observed in %d attempts", attempts)
}
