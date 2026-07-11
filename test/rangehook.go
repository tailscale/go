// run

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test the runtime.DidRange hook, which the compiler arranges to be
// called after every range loop with the potential (max) number of
// iterations and the actual number of iterations.

package main

import (
	"fmt"
	"os"
	"runtime"
)

type event struct {
	label string
	pot   int
	iters int
}

var events []event
var label string

func main() {
	events = make([]event, 0, 128)
	runtime.DidRange = func(potentialSize, iterations int) {
		events = append(events, event{label, potentialSize, iterations})
	}

	check := func(name string, wantPot, wantIters int, f func()) {
		label = name
		start := len(events)
		f()
		label = ""
		got := events[start:]
		if len(got) != 1 || got[0].pot != wantPot || got[0].iters != wantIters {
			panic(fmt.Sprintf("%s: got %+v, want 1 event (pot=%d, iters=%d)", name, got, wantPot, wantIters))
		}
	}

	s := []int{10, 20, 30, 40, 50}
	check("slice-full", 5, 5, func() {
		sum := 0
		for _, v := range s {
			sum += v
		}
		_ = sum
	})
	check("slice-break", 5, 3, func() {
		for i := range s {
			if i == 2 {
				break
			}
		}
	})
	check("slice-novar", 5, 5, func() {
		n := 0
		for range s {
			n++
		}
		_ = n
	})

	var arr [7]int
	check("array-full", 7, 7, func() {
		for i, v := range arr {
			_ = i
			_ = v
		}
	})

	m := map[string]int{"a": 1, "b": 2, "c": 3}
	check("map-full", 3, 3, func() {
		n := 0
		for k, v := range m {
			_ = k
			n += v
		}
	})
	check("map-clear-idiom", 3, 3, func() {
		for k := range m {
			delete(m, k)
		}
	})

	check("string-ascii", 5, 5, func() {
		for i, r := range "hello" {
			_ = i
			_ = r
		}
	})
	check("string-utf8", 10, 6, func() {
		// 6 runes, 10 bytes.
		for range "héllö世" {
		}
	})

	check("int-full", 10, 10, func() {
		for i := range 10 {
			_ = i
		}
	})
	check("int-break", 10, 4, func() {
		for i := range 10 {
			if i == 3 {
				break
			}
		}
	})
	check("int-typed", 6, 6, func() {
		var n uint8 = 6
		for range n {
		}
	})

	check("chan", -1, 3, func() {
		ch := make(chan int, 3)
		ch <- 1
		ch <- 2
		ch <- 3
		close(ch)
		for v := range ch {
			_ = v
		}
	})

	seq := func(yield func(int) bool) {
		for i := 0; i < 4; i++ {
			if !yield(i) {
				return
			}
		}
	}
	check("iterseq-full", -1, 4, func() {
		for v := range seq {
			_ = v
		}
	})
	check("iterseq-break", -1, 2, func() {
		for v := range seq {
			if v == 1 {
				break
			}
		}
	})

	// A return out of a rangefunc loop still fires the hook.
	label = "iterseq-return"
	start := len(events)
	func() {
		for v := range seq {
			if v == 2 {
				return
			}
		}
	}()
	label = ""
	got := events[start:]
	if len(got) != 1 || got[0].pot != -1 || got[0].iters != 3 {
		panic(fmt.Sprintf("iterseq-return: got %+v", got))
	}

	// The slice-zeroing idiom must still be counted.
	z := make([]int, 9)
	check("slice-zero-idiom", 9, 9, func() {
		for i := range z {
			z[i] = 0
		}
	})

	// Nested loops with labeled break: the inner loop fires per completed
	// inner loop; a labeled break to the outer loop skips the inner loop's
	// hook but still fires the outer loop's hook.
	label = "nested"
	start = len(events)
outer:
	for i := range 3 {
		for j := range 4 {
			if i == 1 && j == 1 {
				break outer
			}
		}
	}
	label = ""
	got = events[start:]
	want := []event{{"nested", 4, 4}, {"nested", 3, 2}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		panic(fmt.Sprintf("nested: got %+v, want %+v", got, want))
	}

	// Reentrancy guard: a hook that itself executes range loops
	// (directly or via fmt) must not recurse.
	calls := 0
	runtime.DidRange = func(potentialSize, iterations int) {
		calls++
		fmt.Fprintf(os.Stderr, "") // fmt internals contain range loops
		for range 2 {              // and so does the hook itself
		}
	}
	for range 5 {
	}
	if calls != 1 {
		panic(fmt.Sprintf("reentrancy: hook called %d times, want 1", calls))
	}
}
