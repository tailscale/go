// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2_test

import (
	"internal/testenv"
	"testing"
)

// TestH2InteropBuilds checks that the h2interop interop test program
// still compiles. It's excluded from normal builds by a build tag.
func TestH2InteropBuilds(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	cmd := testenv.Command(t, testenv.GoToolPath(t), "vet", "-tags=h2interop", "net/http/internal/http2/h2interop")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", cmd, err, out)
	}
}
