// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// GOMAXPROCS=10 go test

package sync_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"os/exec"
	"runtime"
	"strings"
	. "sync"
	"testing"
	"time"
)

func HammerSemaphore(s *uint32, loops int, cdone chan bool) {
	for i := 0; i < loops; i++ {
		Runtime_Semacquire(s)
		Runtime_Semrelease(s, false, 0)
	}
	cdone <- true
}

func TestSemaphore(t *testing.T) {
	s := new(uint32)
	*s = 1
	c := make(chan bool)
	for i := 0; i < 10; i++ {
		go HammerSemaphore(s, 1000, c)
	}
	for i := 0; i < 10; i++ {
		<-c
	}
}

func BenchmarkUncontendedSemaphore(b *testing.B) {
	s := new(uint32)
	*s = 1
	HammerSemaphore(s, b.N, make(chan bool, 2))
}

func BenchmarkContendedSemaphore(b *testing.B) {
	b.StopTimer()
	s := new(uint32)
	*s = 1
	c := make(chan bool)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	b.StartTimer()

	go HammerSemaphore(s, b.N/2, c)
	go HammerSemaphore(s, b.N/2, c)
	<-c
	<-c
}

func HammerMutex(m *Mutex, loops int, cdone chan bool) {
	for i := 0; i < loops; i++ {
		m.Lock()
		m.Unlock()
	}
	cdone <- true
}

func wantPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		recover()
	}()
	fn()
	t.Fatal("failed to panic")
}

type lockAsserter interface {
	Lock()
	AssertLocked()
	Unlock()
}

func testAssertLocked(t *testing.T, m lockAsserter) {
	wantPanic(t, func() { m.AssertLocked() })
	m.Lock()
	m.AssertLocked()
	m.Unlock()
	wantPanic(t, func() { m.AssertLocked() })
	// Test correct handling of mutex with waiter.
	m.Lock()
	m.AssertLocked()
	go func() {
		m.Lock()
		m.Unlock()
	}()
	// Give the goroutine above a few moments to get started.
	// The test will pass whether or not we win the race,
	// but we want to run sometimes, to get the test coverage.
	time.Sleep(10 * time.Millisecond)
	m.AssertLocked()
}

func TestAssertLocked(t *testing.T) {
	t.Run("Mutex", func(t *testing.T) { testAssertLocked(t, new(Mutex)) })
	t.Run("RWMutex", func(t *testing.T) {
		t.Run("Lock", func(t *testing.T) { testAssertLocked(t, new(RWMutex)) })
		t.Run("RLock", func(t *testing.T) {
			m := new(RWMutex)
			wantPanic(t, func() { m.AssertRLocked() })

			m.Lock()
			m.AssertRLocked()
			m.Unlock()

			m.RLock()
			m.AssertRLocked()
			m.RUnlock()

			wantPanic(t, func() { m.AssertRLocked() })

			// Test correct handling of mutex with waiter.
			m.RLock()
			m.AssertRLocked()
			go func() {
				m.RLock()
				m.RUnlock()
			}()
			// Give the goroutine above a few moments to get started.
			// The test will pass whether or not we win the race,
			// but we want to run sometimes, to get the test coverage.
			time.Sleep(10 * time.Millisecond)
			m.AssertRLocked()
			m.RUnlock()

			// Test correct handling of rlock with write waiter.
			m.RLock()
			m.AssertRLocked()
			go func() {
				m.Lock()
				m.Unlock()
			}()
			// Give the goroutine above a few moments to get started.
			// The test will pass whether or not we win the race,
			// but we want to run sometimes, to get the test coverage.
			time.Sleep(10 * time.Millisecond)
			m.AssertRLocked()
			m.RUnlock()

			// Test correct handling of rlock with other rlocks.
			// This is a bit racy, but losing the race hurts nothing,
			// and winning the race means correct test coverage.
			m.RLock()
			m.AssertRLocked()
			go func() {
				m.RLock()
				time.Sleep(10 * time.Millisecond)
				m.RUnlock()
			}()
			time.Sleep(5 * time.Millisecond)
			m.AssertRLocked()
			m.RUnlock()
		})
	})
}

func TestMutex(t *testing.T) {
	if n := runtime.SetMutexProfileFraction(1); n != 0 {
		t.Logf("got mutexrate %d expected 0", n)
	}
	defer runtime.SetMutexProfileFraction(0)
	m := new(Mutex)
	c := make(chan bool)
	for i := 0; i < 10; i++ {
		go HammerMutex(m, 1000, c)
	}
	for i := 0; i < 10; i++ {
		<-c
	}
}

var misuseTests = []struct {
	name string
	f    func()
}{
	{
		"Mutex.Unlock",
		func() {
			var mu Mutex
			mu.Unlock()
		},
	},
	{
		"Mutex.Unlock2",
		func() {
			var mu Mutex
			mu.Lock()
			mu.Unlock()
			mu.Unlock()
		},
	},
	{
		"RWMutex.Unlock",
		func() {
			var mu RWMutex
			mu.Unlock()
		},
	},
	{
		"RWMutex.Unlock2",
		func() {
			var mu RWMutex
			mu.RLock()
			mu.Unlock()
		},
	},
	{
		"RWMutex.Unlock3",
		func() {
			var mu RWMutex
			mu.Lock()
			mu.Unlock()
			mu.Unlock()
		},
	},
	{
		"RWMutex.RUnlock",
		func() {
			var mu RWMutex
			mu.RUnlock()
		},
	},
	{
		"RWMutex.RUnlock2",
		func() {
			var mu RWMutex
			mu.Lock()
			mu.RUnlock()
		},
	},
	{
		"RWMutex.RUnlock3",
		func() {
			var mu RWMutex
			mu.RLock()
			mu.RUnlock()
			mu.RUnlock()
		},
	},
}

func init() {
	if len(os.Args) == 3 && os.Args[1] == "TESTMISUSE" {
		for _, test := range misuseTests {
			if test.name == os.Args[2] {
				func() {
					defer func() { recover() }()
					test.f()
				}()
				fmt.Printf("test completed\n")
				os.Exit(0)
			}
		}
		fmt.Printf("unknown test\n")
		os.Exit(0)
	}
}

func TestMutexMisuse(t *testing.T) {
	testenv.MustHaveExec(t)
	for _, test := range misuseTests {
		out, err := exec.Command(os.Args[0], "TESTMISUSE", test.name).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "unlocked") {
			t.Errorf("%s: did not find failure with message about unlocked lock: %s\n%s\n", test.name, err, out)
		}
	}
}

func TestMutexFairness(t *testing.T) {
	var mu Mutex
	stop := make(chan bool)
	defer close(stop)
	go func() {
		for {
			mu.Lock()
			time.Sleep(100 * time.Microsecond)
			mu.Unlock()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	done := make(chan bool)
	go func() {
		for i := 0; i < 10; i++ {
			time.Sleep(100 * time.Microsecond)
			mu.Lock()
			mu.Unlock()
		}
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("can't acquire Mutex in 10 seconds")
	}
}

func BenchmarkMutexUncontended(b *testing.B) {
	type PaddedMutex struct {
		Mutex
		pad [128]uint8
	}
	b.RunParallel(func(pb *testing.PB) {
		var mu PaddedMutex
		for pb.Next() {
			mu.Lock()
			mu.Unlock()
		}
	})
}

func benchmarkMutex(b *testing.B, slack, work bool) {
	var mu Mutex
	if slack {
		b.SetParallelism(10)
	}
	b.RunParallel(func(pb *testing.PB) {
		foo := 0
		for pb.Next() {
			mu.Lock()
			mu.Unlock()
			if work {
				for i := 0; i < 100; i++ {
					foo *= 2
					foo /= 2
				}
			}
		}
		_ = foo
	})
}

func BenchmarkMutex(b *testing.B) {
	benchmarkMutex(b, false, false)
}

func BenchmarkMutexSlack(b *testing.B) {
	benchmarkMutex(b, true, false)
}

func BenchmarkMutexWork(b *testing.B) {
	benchmarkMutex(b, false, true)
}

func BenchmarkMutexWorkSlack(b *testing.B) {
	benchmarkMutex(b, true, true)
}

func BenchmarkMutexNoSpin(b *testing.B) {
	// This benchmark models a situation where spinning in the mutex should be
	// non-profitable and allows to confirm that spinning does not do harm.
	// To achieve this we create excess of goroutines most of which do local work.
	// These goroutines yield during local work, so that switching from
	// a blocked goroutine to other goroutines is profitable.
	// As a matter of fact, this benchmark still triggers some spinning in the mutex.
	var m Mutex
	var acc0, acc1 uint64
	b.SetParallelism(4)
	b.RunParallel(func(pb *testing.PB) {
		c := make(chan bool)
		var data [4 << 10]uint64
		for i := 0; pb.Next(); i++ {
			if i%4 == 0 {
				m.Lock()
				acc0 -= 100
				acc1 += 100
				m.Unlock()
			} else {
				for i := 0; i < len(data); i += 4 {
					data[i]++
				}
				// Elaborate way to say runtime.Gosched
				// that does not put the goroutine onto global runq.
				go func() {
					c <- true
				}()
				<-c
			}
		}
	})
}

func BenchmarkMutexSpin(b *testing.B) {
	// This benchmark models a situation where spinning in the mutex should be
	// profitable. To achieve this we create a goroutine per-proc.
	// These goroutines access considerable amount of local data so that
	// unnecessary rescheduling is penalized by cache misses.
	var m Mutex
	var acc0, acc1 uint64
	b.RunParallel(func(pb *testing.PB) {
		var data [16 << 10]uint64
		for i := 0; pb.Next(); i++ {
			m.Lock()
			acc0 -= 100
			acc1 += 100
			m.Unlock()
			for i := 0; i < len(data); i += 4 {
				data[i]++
			}
		}
	})
}
