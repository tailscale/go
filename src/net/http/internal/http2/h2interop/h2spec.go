// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// h2spec (https://github.com/summerwind/h2spec) is a conformance
// test suite for HTTP/2 servers, covering RFC 7540 and RFC 7541. We
// run it, in strict mode, against the Go server through the tap, and
// report each h2spec case as a sub-result named after its section
// (e.g. "h2spec/tls/http2/6.5.3/1").
//
// Note that h2spec predates RFC 9113, which relaxed or changed some
// requirements; known.txt records the resulting expected failures.

func init() {
	for _, mode := range []string{"tls", "h2c"} {
		extraTests = append(extraTests, &Test{
			Name:    "h2spec/" + mode,
			Desc:    "h2spec conformance suite against the Go server",
			Peer:    "h2spec",
			Images:  []string{"h2spec"},
			Timeout: 5 * time.Minute,
			Run: func(ctx context.Context, tc *testCtx) {
				runH2spec(ctx, tc, mode)
			},
		})
	}
}

// h2specReqs maps h2spec cases (by section path and number, as in
// the sub-test names) to the requirements they cover.
var h2specReqs = map[string]string{
	"generic/2/1":     "9113-6.3-d1",
	"generic/2/2":     "9113-6.9-d1 9113-6.9-4",
	"generic/2/3":     "9113-6.3-d1",
	"generic/2/5":     "9113-6.3-d1",
	"generic/3.1/3":   "9113-6.1-1",
	"generic/3.10/2":  "9113-6.10-d1",
	"generic/5/1":     "7541-3.2-1",
	"generic/5/14":    "7541-3.2-1",
	"generic/5/15":    "7541-4.2-d1 7541-3.2-1",
	"hpack/2.3.3/1":   "9113-4.3-3 7541-2.3.3-1",
	"hpack/2.3.3/2":   "7541-2.3.3-1",
	"hpack/4.2/1":     "9113-4.3-3 7541-4.2-2",
	"hpack/5.2/1":     "7541-5.2-1",
	"hpack/5.2/2":     "7541-5.2-2",
	"hpack/5.2/3":     "9113-4.3-3 7541-5.2-3",
	"hpack/6.1/1":     "7541-6.1-1",
	"hpack/6.3/1":     "9113-4.3-3 7541-6.3-2",
	"http2/3.5/2":     "9113-3.4-4",
	"http2/4.1/1":     "9113-4.1-2",
	"http2/4.1/2":     "9113-4.1-3",
	"http2/4.1/3":     "9113-4.1-4",
	"http2/4.2/1":     "9113-4.2-1",
	"http2/4.2/2":     "9113-4.2-2",
	"http2/4.3/1":     "9113-4.3-2",
	"http2/5.1.1/1":   "9113-6.8-d1 9113-5.1-2 9113-5.1.1-3",
	"http2/5.1.1/2":   "9113-5.1.1-3",
	"http2/5.1.2/1":   "9113-5.1.2-2",
	"http2/5.1/1":     "9113-5.1-1 9113-5.1-14",
	"http2/5.1/2":     "9113-5.1-1",
	"http2/5.1/3":     "9113-5.1-1",
	"http2/5.1/4":     "9113-5.1-14",
	"http2/5.1/5":     "9113-5.1-8",
	"http2/5.1/6":     "9113-5.1-8",
	"http2/5.1/7":     "9113-5.1-8",
	"http2/5.4.1/1":   "9113-5.4.1-2",
	"http2/5.4.1/2":   "9113-5.4-5 9113-5.4.1-1",
	"http2/5.5/1":     "9113-5.5-1 9113-5.5-2",
	"http2/5.5/2":     "9113-5.5-3",
	"http2/6.1/1":     "9113-6.1-5",
	"http2/6.1/2":     "9113-6.1-6",
	"http2/6.1/3":     "9113-6.1-7",
	"http2/6.10/2":    "9113-6.10-2",
	"http2/6.10/3":    "9113-6.10-4",
	"http2/6.10/4":    "9113-6.10-6",
	"http2/6.10/5":    "9113-6.10-6",
	"http2/6.10/6":    "9113-6.10-6",
	"http2/6.2/1":     "9113-6.2-4",
	"http2/6.2/2":     "9113-6.2-4",
	"http2/6.2/3":     "9113-6.2-6",
	"http2/6.2/4":     "9113-6.2-7",
	"http2/6.3/1":     "9113-6.3-1",
	"http2/6.3/2":     "9113-6.3-2",
	"http2/6.4/1":     "9113-6.4-4",
	"http2/6.4/2":     "9113-6.4-6",
	"http2/6.4/3":     "9113-4.2-2 9113-6.4-7",
	"http2/6.5.2/1":   "9113-6.5-2 9113-6.5.2-3",
	"http2/6.5.2/2":   "9113-6.5.2-9",
	"http2/6.5.2/3":   "9113-6.5-2 9113-6.5.2-11",
	"http2/6.5.2/4":   "9113-6.5.2-11",
	"http2/6.5.2/5":   "9113-5.5-1 9113-6.5.2-13 9113-6.5.3-3",
	"http2/6.5.3/1":   "9113-6.5.3-2",
	"http2/6.5.3/2":   "9113-3.4-3 9113-6.5.3-4",
	"http2/6.5/1":     "9113-6.5-4",
	"http2/6.5/2":     "9113-6.5-6",
	"http2/6.5/3":     "9113-6.5-7 9113-6.5-8",
	"http2/6.7/1":     "9113-6.7-2 9113-6.7-4",
	"http2/6.7/2":     "9113-6.7-5",
	"http2/6.7/3":     "9113-6.7-6",
	"http2/6.7/4":     "9113-4.2-2 9113-6.7-7",
	"http2/6.8/1":     "9113-6.8-4",
	"http2/6.9.1/2":   "9113-6.9.1-4",
	"http2/6.9.1/3":   "9113-6.9.1-4",
	"http2/6.9.2/1":   "9113-6.5.3-1 9113-6.9.2-1",
	"http2/6.9.2/2":   "9113-6.9.2-2",
	"http2/6.9/1":     "9113-6.9-3",
	"http2/6.9/2":     "9113-6.9-3",
	"http2/6.9/3":     "9113-4.2-2 9113-6.9-6",
	"http2/7/1":       "9113-5.5-1 9113-7-1",
	"http2/7/2":       "9113-7-1",
	"http2/8.1.2.1/1": "9113-8.3-4",
	"http2/8.1.2.1/2": "9113-8.3-4",
	"http2/8.1.2.1/3": "9113-8.1-4",
	"http2/8.1.2.1/4": "9113-8.3-6",
	"http2/8.1.2.2/1": "9113-8.2.2-2",
	"http2/8.1.2.2/2": "9113-8.2.2-3",
	"http2/8.1.2.3/1": "9113-8.3.1-10",
	"http2/8.1.2.3/2": "9113-8.3.1-12",
	"http2/8.1.2.3/3": "9113-8.3.1-12",
	"http2/8.1.2.3/4": "9113-8.3.1-12",
	"http2/8.1.2.3/5": "9113-8.3-8 9113-8.3.1-12",
	"http2/8.1.2.3/6": "9113-8.3-8 9113-8.3.1-12",
	"http2/8.1.2.3/7": "9113-8.3-8 9113-8.3.1-12",
	"http2/8.1.2.6/1": "9113-8.1.1-d1",
	"http2/8.1.2.6/2": "9113-8.1.1-d1",
	"http2/8.1.2/1":   "9113-8.1.1-3 9113-8.2.1-3",
	"http2/8.1/1":     "9113-8.1-5",
	"http2/8.2/1":     "9113-6.6-7 9113-6.6-9 9113-8.4-7",
}

type junitReport struct {
	Suites []struct {
		Name    string `xml:"name,attr"`
		Package string `xml:"package,attr"`
		Cases   []struct {
			Package   string    `xml:"package,attr"`
			ClassName string    `xml:"classname,attr"`
			Error     *string   `xml:"error"`
			Failure   *string   `xml:"failure"`
			Skipped   *struct{} `xml:"skipped"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

func runH2spec(ctx context.Context, tc *testCtx, mode string) {
	tc.peerFindingsIgnored = true // h2spec misbehaves on purpose
	srv := &goServer{profile: goProfiles[0]}
	addr, err := srv.Start(ctx, tc, mode)
	if err != nil {
		tc.Errorf("starting Go server: %v", err)
		return
	}
	tap, err := tc.Tap(addr, mode == "tls", "h2spec client", "Go server")
	if err != nil {
		tc.Errorf("starting tap: %v", err)
		return
	}
	outDir, err := os.MkdirTemp("", "h2interop-h2spec-")
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	defer os.RemoveAll(outDir)
	os.Chmod(outDir, 0o777)

	_, port, _ := strings.Cut(tap.Addr(), ":")
	args := []string{"-h", "127.0.0.1", "-p", port, "--strict", "-j", "/out/report.xml", "-o", "5"}
	if mode == "tls" {
		args = append(args, "-t", "-k")
	}
	out, err := tc.env.Docker.RunOnce(ctx, "h2spec", []string{"-v", outDir + ":/out"}, args...)
	tc.Logf("h2spec output:\n%s", out)
	data, rerr := os.ReadFile(filepath.Join(outDir, "report.xml"))
	if rerr != nil {
		tc.Errorf("h2spec produced no report (%v): %v", err, rerr)
		return
	}
	var rep junitReport
	if err := xml.Unmarshal(data, &rep); err != nil {
		tc.Errorf("parsing h2spec report: %v", err)
		return
	}
	failed := 0
	for _, s := range rep.Suites {
		for i, c := range s.Cases {
			name := fmt.Sprintf("%s/%d", c.Package, i+1)
			switch {
			case c.Skipped != nil:
				tc.SubResult(name, Skip, c.ClassName)
			case c.Error != nil || c.Failure != nil:
				msg := ""
				if c.Error != nil {
					msg = *c.Error
				} else {
					msg = *c.Failure
				}
				// h2spec's report separates the expected and actual
				// results only by position; the last line is the actual.
				lines := strings.Split(strings.TrimSpace(msg), "\n")
				act := lines[len(lines)-1]
				exp := strings.Join(lines[:len(lines)-1], " or ")
				tc.SubResult(name, Fail, fmt.Sprintf("%s: expected %s; got %s", c.ClassName, exp, act))
				failed++
			default:
				tc.SubResult(name, Pass)
			}
		}
	}
	tc.Logf("%d h2spec cases failed", failed)
}
