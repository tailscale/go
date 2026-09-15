// Copyright 2024 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"testing"
)

func checkStackStats(t *testing.T, what string, m *runtime.TailscaleStackStats) {
	t.Helper()
	t.Logf("%s: %+v", what, *m)
	if m.Size == 0 || bits.OnesCount64(m.Size) != 1 {
		t.Errorf("%s: Size = %d; want a nonzero power of two", what, m.Size)
	}
	if m.Used == 0 || m.Used > m.Size {
		t.Errorf("%s: Used = %d; want in (0, %d]", what, m.Used, m.Size)
	}
	if m.MaxSize < m.Size || bits.OnesCount64(m.MaxSize) != 1 {
		t.Errorf("%s: MaxSize = %d; want a power of two >= Size (%d)", what, m.MaxSize, m.Size)
	}
}

// tsUseStack recursively consumes at least n bytes of stack and then
// reads the stack stats from the deepest frame.
//
//go:noinline
func tsUseStack(n int, m *runtime.TailscaleStackStats) byte {
	var buf [256]byte
	if n <= 0 {
		runtime.TailscaleReadStackStats(m)
		return buf[0]
	}
	buf[n%len(buf)] = byte(n)
	return tsUseStack(n-len(buf), m) + buf[n%len(buf)]
}

func TestTailscaleReadStackStats(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)

		var initial runtime.TailscaleStackStats
		runtime.TailscaleReadStackStats(&initial)
		checkStackStats(t, "initial", &initial)
		if initial.Growths != 0 || initial.Shrinks != 0 {
			t.Errorf("new goroutine: Growths=%d Shrinks=%d; want 0, 0", initial.Growths, initial.Shrinks)
		}
		if initial.MaxSize != initial.Size {
			t.Errorf("new goroutine: MaxSize=%d != Size=%d", initial.MaxSize, initial.Size)
		}

		// Recurse deep enough to force the stack to grow several times.
		var deep runtime.TailscaleStackStats
		tsUseStack(int(initial.Size)*8, &deep)
		checkStackStats(t, "deep", &deep)
		if deep.Size <= initial.Size {
			t.Errorf("after recursion: Size=%d; want > initial %d", deep.Size, initial.Size)
		}
		if deep.Used <= initial.Used {
			t.Errorf("after recursion: Used=%d; want > initial %d", deep.Used, initial.Used)
		}
		if deep.Growths == 0 {
			t.Errorf("after recursion: Growths=0; want > 0")
		}
		if deep.MaxSize != deep.Size {
			t.Errorf("after recursion: MaxSize=%d != Size=%d", deep.MaxSize, deep.Size)
		}

		// Back at the shallow frame, the stack is still large until
		// the GC shrinks it. Trigger a few GCs so it does.
		for range 5 {
			runtime.GC()
		}
		var shrunk runtime.TailscaleStackStats
		runtime.TailscaleReadStackStats(&shrunk)
		checkStackStats(t, "after GC", &shrunk)
		if shrunk.Growths != deep.Growths {
			t.Errorf("after GC: Growths=%d; want unchanged %d", shrunk.Growths, deep.Growths)
		}
		if shrunk.MaxSize != deep.Size {
			t.Errorf("after GC: MaxSize=%d; want peak size %d", shrunk.MaxSize, deep.Size)
		}
		if shrunk.Size >= deep.Size {
			t.Errorf("after GC: Size=%d; want < peak %d", shrunk.Size, deep.Size)
		}
		if shrunk.Shrinks == 0 {
			t.Errorf("after GC: Shrinks=0; want > 0")
		}
	}()
	<-done

	// A fresh goroutine, which very likely reuses the g just freed
	// above, must start with clean counters.
	done = make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.TailscaleStackStats
		runtime.TailscaleReadStackStats(&m)
		checkStackStats(t, "reused", &m)
		if m.Growths != 0 || m.Shrinks != 0 || m.MaxSize != m.Size {
			t.Errorf("reused goroutine: %+v; want zero Growths/Shrinks and MaxSize == Size", m)
		}
	}()
	<-done
}

func BenchmarkTailscaleReadStackStats(b *testing.B) {
	var m runtime.TailscaleStackStats
	for range b.N {
		runtime.TailscaleReadStackStats(&m)
	}
}

const stackHistMetric = "/tailscale/sched/goroutines-by-stack-size:bytes"

// readStackHist reads the goroutine stack size histogram metric.
func readStackHist(tb testing.TB) *metrics.Float64Histogram {
	tb.Helper()
	s := []metrics.Sample{{Name: stackHistMetric}}
	metrics.Read(s)
	if kind := s[0].Value.Kind(); kind != metrics.KindFloat64Histogram {
		tb.Fatalf("%s: kind = %v; want KindFloat64Histogram", stackHistMetric, kind)
	}
	return s[0].Value.Float64Histogram()
}

// stackHistCountAtLeast returns the number of goroutines that h reports
// as having a stack of at least size bytes.
func stackHistCountAtLeast(h *metrics.Float64Histogram, size uint64) (n uint64) {
	for i, c := range h.Counts {
		if h.Buckets[i] >= float64(size) {
			n += c
		}
	}
	return n
}

// checkStackHist requires that the incrementally maintained stack size
// histogram exactly matches one computed by walking every goroutine
// with the world stopped.
func checkStackHist(t *testing.T, what string) {
	t.Helper()
	want, got := runtime.TailscaleStackHistSlow()
	if !slices.Equal(want, got) {
		t.Errorf("%s: stack size histogram is out of sync with the goroutines\n got: %v\nwant: %v", what, got, want)
		return
	}
	t.Logf("%s: %v", what, got)
}

// tsParkDeep recursively consumes at least n bytes of stack, then calls
// parked.Done and blocks until release is closed, so that the goroutine
// stays parked with its grown stack.
//
//go:noinline
func tsParkDeep(n int, parked *sync.WaitGroup, release <-chan struct{}) byte {
	var buf [256]byte
	if n <= 0 {
		parked.Done()
		<-release
		return buf[0]
	}
	buf[n%len(buf)] = byte(n)
	return tsParkDeep(n-len(buf), parked, release) + buf[n%len(buf)]
}

func TestTailscaleStackHistMetric(t *testing.T) {
	hist := readStackHist(t)
	if len(hist.Counts) != len(hist.Buckets)-1 {
		t.Fatalf("len(Counts) = %d, len(Buckets) = %d; want Counts to be one shorter", len(hist.Counts), len(hist.Buckets))
	}
	if len(hist.Buckets) < 2 {
		t.Fatalf("Buckets = %v; want at least two boundaries", hist.Buckets)
	}
	bounds := hist.Buckets[:len(hist.Buckets)-1]
	for i, b := range bounds {
		if b <= 0 || b != math.Trunc(b) || bits.OnesCount64(uint64(b)) != 1 {
			t.Errorf("Buckets[%d] = %v; want a power of two", i, b)
		}
		if i > 0 && b != 2*bounds[i-1] {
			t.Errorf("Buckets[%d] = %v; want double Buckets[%d] = %v", i, b, i-1, bounds[i-1])
		}
	}
	if last := hist.Buckets[len(hist.Buckets)-1]; !math.IsInf(last, 1) {
		t.Errorf("last bucket boundary = %v; want +Inf", last)
	}

	// This goroutine's own stack must be counted in the bucket for
	// its size.
	var st runtime.TailscaleStackStats
	runtime.TailscaleReadStackStats(&st)
	if i := slices.Index(bounds, float64(st.Size)); i < 0 {
		t.Errorf("no bucket for this goroutine's stack size %d in %v", st.Size, bounds)
	} else if hist.Counts[i] == 0 {
		t.Errorf("bucket for this goroutine's stack size %d is empty", st.Size)
	}

	// The bucket counts sum to the goroutine count. Both are read
	// without stopping the world, so retry in case goroutines came or
	// went in between.
	s := []metrics.Sample{{Name: stackHistMetric}, {Name: "/sched/goroutines:goroutines"}}
	for attempt := 1; ; attempt++ {
		metrics.Read(s)
		var sum uint64
		for _, c := range s[0].Value.Float64Histogram().Counts {
			sum += c
		}
		if sum == s[1].Value.Uint64() {
			break
		}
		if attempt == 20 {
			t.Fatalf("histogram sums to %d goroutines, /sched/goroutines:goroutines = %d; never matched", sum, s[1].Value.Uint64())
		}
		t.Logf("histogram sums to %d goroutines, /sched/goroutines:goroutines = %d; retrying", sum, s[1].Value.Uint64())
	}
}

func TestTailscaleStackHistTracking(t *testing.T) {
	checkStackHist(t, "initial")

	release := make(chan struct{})
	var wg sync.WaitGroup

	// Create goroutines that park immediately with their initial
	// stacks.
	const idle = 500
	for range idle {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
		}()
	}
	checkStackHist(t, "after creating goroutines")

	// Grow some stacks and keep them grown by parking deep in the
	// recursion.
	const grown = 10
	const growTo = 64 << 10
	before := stackHistCountAtLeast(readStackHist(t), growTo)
	var parked sync.WaitGroup
	for range grown {
		wg.Add(1)
		parked.Add(1)
		go func() {
			defer wg.Done()
			tsParkDeep(growTo, &parked, release)
		}()
	}
	parked.Wait()
	checkStackHist(t, "after growing stacks")
	if n := stackHistCountAtLeast(readStackHist(t), growTo); n < before+grown {
		t.Errorf("goroutines with stacks of at least %d bytes: %d; want at least %d", growTo, n, before+grown)
	}

	// A stack larger than the histogram's largest exact bucket lands
	// in the catch-all last bucket. The last exact bucket is 16 MiB,
	// so using 32 MiB of stack, which needs a 64 MiB stack, gets there
	// with room to spare.
	hist := readStackHist(t)
	catchAll := uint64(hist.Buckets[len(hist.Buckets)-2])
	wg.Add(1)
	parked.Add(1)
	go func() {
		defer wg.Done()
		tsParkDeep(int(catchAll), &parked, release)
	}()
	parked.Wait()
	checkStackHist(t, "after growing one stack past the largest bucket")
	if hist = readStackHist(t); hist.Counts[len(hist.Counts)-1] == 0 {
		t.Errorf("catch-all bucket [%d, +Inf) is empty; want the goroutine using %d bytes of stack", catchAll, catchAll)
	}

	// Grow other stacks and then return to a shallow frame before
	// parking, so that the garbage collector shrinks them.
	for range grown {
		wg.Add(1)
		parked.Add(1)
		go func() {
			defer wg.Done()
			var m runtime.TailscaleStackStats
			tsUseStack(growTo, &m)
			parked.Done()
			<-release
		}()
	}
	parked.Wait()
	beforeGC := stackHistCountAtLeast(readStackHist(t), growTo)
	for range 5 {
		runtime.GC()
	}
	checkStackHist(t, "after GC shrank stacks")
	if afterGC := stackHistCountAtLeast(readStackHist(t), growTo); beforeGC < before+2*grown {
		t.Logf("skipping shrink check: only %d of %d grown stacks were still large before GC", beforeGC-before, 2*grown)
	} else if afterGC >= beforeGC {
		t.Errorf("goroutines with stacks of at least %d bytes after GC: %d; want fewer than %d", growTo, afterGC, beforeGC)
	}

	// Changing GOMAXPROCS destroys and creates Ps. The counts held by
	// destroyed Ps must survive.
	procs := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(procs)
	runtime.GOMAXPROCS(1)
	checkStackHist(t, "after GOMAXPROCS(1)")
	runtime.GOMAXPROCS(procs + 2)
	checkStackHist(t, "after growing GOMAXPROCS")
	runtime.GOMAXPROCS(procs)

	close(release)
	wg.Wait()
	checkStackHist(t, "after goroutines exited")

	// The 64 MiB stack skewed the average stack size that the runtime
	// starts new goroutines with (see gcComputeStartingStackSize), and
	// it stays skewed until the next GC recomputes it from the
	// goroutines that are still alive. Do that now rather than leaving
	// every goroutine the next test creates with a huge stack.
	runtime.GC()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.TailscaleStackStats
		runtime.TailscaleReadStackStats(&m)
		t.Logf("fresh goroutine after GC: %+v", m)
	}()
	<-done
}

// TestTailscaleStackHistCacheLine checks that the per-P histogram
// counters share a cache line with goroutinesCreated, which goroutine
// creation writes anyway, so that maintaining the histogram dirties no
// extra cache line. The layout differs on 32-bit platforms, where the
// runtime is not tuned this carefully, so only 64-bit ones are checked.
// If an upstream change to the p struct breaks this, move tsStackHist
// so that it is whole within goroutinesCreated's line again.
func TestTailscaleStackHistCacheLine(t *testing.T) {
	if bits.UintSize != 64 {
		t.Skip("layout is only tuned for 64-bit platforms")
	}
	const line = 64
	histOff, histSize, createdOff := runtime.TailscaleStackHistLayout()
	t.Logf("p.tsStackHist at offset %d, %d bytes; p.goroutinesCreated at offset %d", histOff, histSize, createdOff)
	if histOff/line != (histOff+histSize-1)/line {
		t.Errorf("p.tsStackHist straddles a %d-byte cache line", line)
	}
	if histOff/line != createdOff/line {
		t.Errorf("p.tsStackHist and p.goroutinesCreated are in different %d-byte cache lines", line)
	}
}

// readStackCopyMetrics returns the cumulative stack growth, shrink, and
// copied byte counters.
func readStackCopyMetrics(tb testing.TB) (growths, shrinks, copied uint64) {
	tb.Helper()
	s := []metrics.Sample{
		{Name: "/tailscale/sched/stacks/growths:events"},
		{Name: "/tailscale/sched/stacks/shrinks:events"},
		{Name: "/tailscale/sched/stacks/copied:bytes"},
	}
	metrics.Read(s)
	for _, v := range s {
		if kind := v.Value.Kind(); kind != metrics.KindUint64 {
			tb.Fatalf("%s: kind = %v; want KindUint64", v.Name, kind)
		}
	}
	return s[0].Value.Uint64(), s[1].Value.Uint64(), s[2].Value.Uint64()
}

func TestTailscaleStackCopyMetrics(t *testing.T) {
	growths0, shrinks0, copied0 := readStackCopyMetrics(t)

	// Grow a fresh goroutine's stack several times, then let the GC
	// shrink it back. TailscaleReadStackStats reports how many times
	// that one goroutine grew and shrank, and the process-wide
	// counters must have increased by at least that much. The
	// recursion depth is relative to the initial stack size because
	// the runtime adapts the size it starts goroutines with.
	var initial, deep, shrunk runtime.TailscaleStackStats
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.TailscaleReadStackStats(&initial)
		tsUseStack(int(initial.Size)*8, &deep)
		for range 5 {
			runtime.GC()
		}
		runtime.TailscaleReadStackStats(&shrunk)
	}()
	<-done

	growths1, shrinks1, copied1 := readStackCopyMetrics(t)
	t.Logf("goroutine %+v; growths %d -> %d, shrinks %d -> %d, copied %d -> %d",
		shrunk, growths0, growths1, shrinks0, shrinks1, copied0, copied1)
	if deep.Growths == 0 || shrunk.Shrinks == 0 {
		t.Fatalf("test goroutine did not both grow (%d) and shrink (%d) its stack", deep.Growths, shrunk.Shrinks)
	}
	if got, want := growths1-growths0, uint64(deep.Growths); got < want {
		t.Errorf("growths increased by %d; want at least %d", got, want)
	}
	if got, want := shrinks1-shrinks0, uint64(shrunk.Shrinks); got < want {
		t.Errorf("shrinks increased by %d; want at least %d", got, want)
	}
	// The final growth alone copied most of the previous stack, which
	// was half of deep.Size, so a quarter of deep.Size is a safe lower
	// bound on the bytes copied.
	if got, want := copied1-copied0, deep.Size/4; got < want {
		t.Errorf("copied bytes increased by %d; want at least %d", got, want)
	}
}

func BenchmarkTailscaleStackHistMetric(b *testing.B) {
	for _, idle := range []int{0, 10000} {
		b.Run(fmt.Sprintf("idle=%d", idle), func(b *testing.B) {
			release := make(chan struct{})
			var wg sync.WaitGroup
			for range idle {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-release
				}()
			}
			s := []metrics.Sample{{Name: stackHistMetric}}
			b.ResetTimer()
			for range b.N {
				metrics.Read(s)
			}
			b.StopTimer()
			close(release)
			wg.Wait()
		})
	}
}
