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

// tsFuncZombieStats holds one reading of the func zombie lifetime
// metrics.
type tsFuncZombieStats struct {
	created, removed uint64
	lifetime         *metrics.Float64Histogram
}

func readTSFuncZombieStats(t *testing.T) tsFuncZombieStats {
	t.Helper()
	s := []metrics.Sample{
		{Name: "/tailscale/sched/timers/zombies/func/created:timers"},
		{Name: "/tailscale/sched/timers/zombies/func/removed:timers"},
		{Name: "/tailscale/sched/timers/zombies/func/lifetime:gc-cycles"},
	}
	metrics.Read(s)
	for _, v := range s[:2] {
		if kind := v.Value.Kind(); kind != metrics.KindUint64 {
			t.Fatalf("%s: kind = %v; want KindUint64", v.Name, kind)
		}
	}
	if kind := s[2].Value.Kind(); kind != metrics.KindFloat64Histogram {
		t.Fatalf("%s: kind = %v; want KindFloat64Histogram", s[2].Name, kind)
	}
	st := tsFuncZombieStats{
		created:  s[0].Value.Uint64(),
		removed:  s[1].Value.Uint64(),
		lifetime: s[2].Value.Float64Histogram(),
	}
	var sum uint64
	for _, c := range st.lifetime.Counts {
		sum += c
	}
	if sum != st.removed {
		t.Errorf("lifetime histogram counts sum to %d; removed = %d", sum, st.removed)
	}
	return st
}

// lasted returns the number of func zombies whose recorded lifetime, in
// GC cycles, could have been anywhere from lo through hi inclusive: the
// sum of the histogram buckets that overlap that range.
func (st tsFuncZombieStats) lasted(lo, hi uint32) uint64 {
	var n uint64
	h := st.lifetime
	for i, c := range h.Counts {
		if h.Buckets[i+1] > float64(lo) && h.Buckets[i] <= float64(hi) {
			n += c
		}
	}
	return n
}

// tsPaddedStoppedTimers creates n stopped AfterFunc timers, each buried
// among four live ones as in TestTailscaleTimerMetricsFunc, so that the
// zombies stay under a quarter of the heap and are not swept before the
// test reads the metrics. It appends the live timers to *live for the
// caller to stop later and returns the stopped ones.
func tsPaddedStoppedTimers(t *testing.T, n int, live *[]*time.Timer) []*time.Timer {
	t.Helper()
	var stopped []*time.Timer
	for range n {
		for range 4 {
			*live = append(*live, time.AfterFunc(time.Hour, func() {}))
		}
		stopped = append(stopped, time.AfterFunc(time.Hour, func() {}))
	}
	for _, tm := range stopped {
		if !tm.Stop() {
			t.Fatal("Stop = false; want true")
		}
	}
	return stopped
}

// TestTailscaleFuncZombieLifetime checks that stopping AfterFunc timers
// counts them as created func zombies, and that resetting them after
// some garbage collections counts them as removed with the right number
// of GC cycles in the lifetime histogram. Any timer add on the same P
// whose earlier deadline has arrived sweeps every zombie out of the
// heap, so an attempt can lose its zombies early; the test retries in
// that case.
func TestTailscaleFuncZombieLifetime(t *testing.T) {
	const n = 20
	const gcs = 3
	var live []*time.Timer
	defer func() {
		for _, tm := range live {
			tm.Stop()
		}
	}()

	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		before := readTSFuncZombieStats(t)
		cycles0 := runtime.TailscaleGCCycles()
		stopped := tsPaddedStoppedTimers(t, n, &live)
		// A second Stop must not disturb the stamp the first one left.
		for _, tm := range stopped {
			if tm.Stop() {
				t.Fatal("second Stop = true; want false")
			}
		}
		mid := readTSFuncZombieStats(t)
		if got := mid.created - before.created; got < n {
			t.Fatalf("created grew by %d after stopping %d timers; want at least %d", got, n, n)
		}

		for range gcs {
			runtime.GC()
		}
		cycles1 := runtime.TailscaleGCCycles()
		if cycles1-cycles0 < gcs {
			t.Fatalf("GC cycles advanced by %d over %d runtime.GC calls", cycles1-cycles0, gcs)
		}

		// Resetting a stopped timer unmarks the zombie and so ends its
		// hold on the function, which is a removal for the histogram.
		for _, tm := range stopped {
			if tm.Reset(time.Hour) {
				t.Fatal("Reset of a stopped timer = true; want false")
			}
		}
		live = append(live, stopped...)
		after := readTSFuncZombieStats(t)
		t.Logf("attempt %d: before: created=%d removed=%d %v", attempt, before.created, before.removed, before.lifetime.Counts)
		t.Logf("attempt %d: after:  created=%d removed=%d %v", attempt, after.created, after.removed, after.lifetime.Counts)

		if got := after.removed - before.removed; got < n {
			t.Fatalf("removed grew by %d after resetting %d stopped timers; want at least %d", got, n, n)
		}
		// Each zombie that survived until its Reset lasted at least gcs
		// cycles and at most however many began during the attempt.
		if got := after.lasted(gcs, cycles1-cycles0) - before.lasted(gcs, cycles1-cycles0); got >= n {
			return
		} else {
			t.Logf("attempt %d: only %d zombies recorded as lasting %d..%d cycles; want %d; retrying", attempt, got, gcs, cycles1-cycles0, n)
		}
	}
	t.Fatalf("no attempt out of %d saw all %d zombies last %d GC cycles", attempts, n, gcs)
}

// TestTailscaleFuncZombieLifetimeImmediate checks that a stopped timer
// reset before the next GC cycle lands in the histogram's first bucket.
func TestTailscaleFuncZombieLifetimeImmediate(t *testing.T) {
	const n = 20
	var live []*time.Timer
	defer func() {
		for _, tm := range live {
			tm.Stop()
		}
	}()
	before := readTSFuncZombieStats(t)
	cycles0 := runtime.TailscaleGCCycles()
	stopped := tsPaddedStoppedTimers(t, n, &live)
	for _, tm := range stopped {
		tm.Reset(time.Hour)
	}
	live = append(live, stopped...)
	after := readTSFuncZombieStats(t)
	if cycles1 := runtime.TailscaleGCCycles(); cycles1 != cycles0 {
		t.Skipf("a GC cycle began during the test (%d -> %d)", cycles0, cycles1)
	}
	if got := after.lifetime.Counts[0] - before.lifetime.Counts[0]; got < n {
		t.Errorf("first lifetime bucket grew by %d after stopping and immediately resetting %d timers; want at least %d", got, n, n)
	}
}

// TestTailscaleFuncZombieRelease checks that TailscaleRelease on an
// already stopped timer records the end of the zombie's lifetime.
func TestTailscaleFuncZombieRelease(t *testing.T) {
	var live []*time.Timer
	defer func() {
		for _, tm := range live {
			tm.Stop()
		}
	}()
	before := readTSFuncZombieStats(t)
	cycles0 := runtime.TailscaleGCCycles()
	stopped := tsPaddedStoppedTimers(t, 1, &live)
	runtime.GC()
	cycles1 := runtime.TailscaleGCCycles()
	if stopped[0].TailscaleRelease() {
		t.Error("TailscaleRelease of a stopped timer = true; want false")
	}
	after := readTSFuncZombieStats(t)
	if got := after.removed - before.removed; got < 1 {
		t.Errorf("removed grew by %d after releasing a stopped timer; want at least 1", got)
	}
	if got := after.lasted(1, cycles1-cycles0) - before.lasted(1, cycles1-cycles0); got < 1 {
		t.Errorf("no zombie recorded as lasting 1..%d cycles after releasing a stopped timer; counts %v -> %v", cycles1-cycles0, before.lifetime.Counts, after.lifetime.Counts)
	}
}

// TestTailscaleFuncZombieFlush checks that the cumulative counts survive
// their P being destroyed by a GOMAXPROCS decrease.
func TestTailscaleFuncZombieFlush(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	if procs < 2 {
		t.Skip("need GOMAXPROCS >= 2 to destroy a P")
	}
	defer runtime.GOMAXPROCS(procs)

	const n = 20
	var live []*time.Timer
	defer func() {
		for _, tm := range live {
			tm.Stop()
		}
	}()
	before := readTSFuncZombieStats(t)
	tsPaddedStoppedTimers(t, n, &live)
	runtime.GOMAXPROCS(1)
	runtime.GOMAXPROCS(procs)
	after := readTSFuncZombieStats(t)
	if got := after.created - before.created; got < n {
		t.Errorf("created grew by %d across a GOMAXPROCS change after stopping %d timers; want at least %d", got, n, n)
	}
	if after.removed < before.removed {
		t.Errorf("removed went from %d to %d across a GOMAXPROCS change", before.removed, after.removed)
	}
}
