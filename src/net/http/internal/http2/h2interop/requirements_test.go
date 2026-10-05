// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import "testing"

// TestRequirements checks that every role of every requirement in
// requirements.txt is covered by a test or marked n/a, and that tests
// only reference known requirements.
func TestRequirements(t *testing.T) {
	reqs, err := parseRequirements(requirementsData)
	if err != nil {
		t.Fatal(err)
	}
	unknown, uncovered := checkRequirements(reqs, declaredCoverage())
	for _, u := range unknown {
		t.Errorf("unknown requirement: %s", u)
	}
	for _, u := range uncovered {
		t.Errorf("uncovered requirement: %s", u)
	}
}
