// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package profilerecord holds internal types used to represent profiling
// records with deep stack traces.
//
// TODO: Consider moving this to internal/runtime, see golang.org/issue/65355.
package profilerecord

import "unsafe"

type StackRecord struct {
	Stack []uintptr
}

// GoroutineRecord describes a single goroutine in an unaggregated
// goroutine dump, as needed for the debug=2 form of the goroutine
// profile. It carries the metadata required to render the classic
// runtime.Stack-style text header for the goroutine.
type GoroutineRecord struct {
	Stack          []uintptr
	Truncated      bool // Stack hit the profile stack depth limit
	Goid           uint64
	ParentGoid     uint64  // goid of creator goroutine, or 0
	Gopc           uintptr // pc of the go statement that created this goroutine, or 0
	Status         string  // e.g. "running", "chan receive"
	WaitMinutes    int64   // approx minutes blocked, or 0
	Leaked         bool    // goroutine was found leaked by the GC
	Durable        bool    // durably blocked in a synctest bubble
	LockedToThread bool
	BubbleID       uint64         // synctest bubble id, or 0
	Labels         unsafe.Pointer // profiler label set, or nil
}

type MemProfileRecord struct {
	ObjectSize                int64
	AllocObjects, FreeObjects int64
	Stack                     []uintptr
}

func (r *MemProfileRecord) InUseBytes() int64   { return r.InUseObjects() * r.ObjectSize }
func (r *MemProfileRecord) InUseObjects() int64 { return r.AllocObjects - r.FreeObjects }

type BlockProfileRecord struct {
	Count  int64
	Cycles int64
	Stack  []uintptr
}
