// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// DidRange is called by every range loop after a range is done. It includes the number
// of times the range looped.
// DidRange is assumed to always be non-nil and may be called concurrently.
// It should not be assigned concurrently with a range call. Set it early in init or main.
// The potentialSize is the max number of iterations it might've looped going into the
// range call, if the body doesn't break. e.g. the len(slice) or len(map) or the integer
// value. For sequences (iter.Seq), potentialSize == -1 (unknown).
var DidRange = func(potentialSize, iterations int) {}

// didRange is called by compiler-generated code after every range loop.
// It guards against reentrancy so that range loops executed by the DidRange
// hook itself (e.g. inside fmt) don't recurse forever.
func didRange(potentialSize, iterations int) {
	gp := getg()
	if gp.inDidRange {
		return
	}
	gp.inDidRange = true
	DidRange(potentialSize, iterations)
	gp.inDidRange = false
}
