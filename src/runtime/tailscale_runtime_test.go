// Copyright 2024 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"math/bits"
	"runtime"
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
