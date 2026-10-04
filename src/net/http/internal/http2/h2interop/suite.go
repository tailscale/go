// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A Test is a single runnable test.
type Test struct {
	Name string
	// Desc describes the test for reports.
	Desc string
	// Peer is the non-Go implementation involved, if any.
	Peer string
	// Images are the docker images the test needs, by peer directory.
	Images []string
	// Timeout overrides the default per-test timeout.
	Timeout time.Duration
	// Run runs the test. It reports failures via tc.
	Run func(ctx context.Context, tc *testCtx)
	// Exclusive tests are not run concurrently with others
	// (e.g. load tests whose timing would perturb others).
	Exclusive bool
}

// Outcome is a test outcome.
type Outcome string

const (
	Pass  Outcome = "PASS"
	Fail  Outcome = "FAIL"  // interop failure or Go spec violation
	Warn  Outcome = "WARN"  // passed, but the peer violated the spec, or SHOULD-level issues
	Skip  Outcome = "SKIP"  // not applicable
	Error Outcome = "ERROR" // test infrastructure failure

	// Flaky is used only in the known-results file, for tests
	// that may pass or fail depending on timing.
	Flaky Outcome = "FLAKY"
)

// TestResult is the result of running a Test.
type TestResult struct {
	Name     string        `json:"name"`
	Peer     string        `json:"peer,omitempty"`
	Outcome  Outcome       `json:"outcome"`
	Failures []string      `json:"failures,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Findings []Finding     `json:"findings,omitempty"` // validator findings at warning level or above
	GoErrors []string      `json:"go_errors,omitempty"`
	Duration time.Duration `json:"duration"`
	Log      string        `json:"-"`
	Trace    string        `json:"-"`

	// Known is the known-results entry matching this test, if any.
	Known *knownEntry `json:"known,omitempty"`
}

// Unexpected reports whether the result differs from what the
// known-results file anticipates.
func (r *TestResult) Unexpected() bool {
	want := Pass
	if r.Known != nil {
		want = r.Known.Outcome
	}
	switch r.Outcome {
	case Skip:
		return false
	case Pass, Warn, Fail:
		if want == Flaky {
			return false
		}
	}
	switch r.Outcome {
	case Pass, Warn:
		return want == Fail || want == Error
	}
	return r.Outcome != want
}

// testCtx is the context for one running test.
type testCtx struct {
	name string
	env  *Env

	mu       sync.Mutex
	log      bytes.Buffer
	failures []string
	warnings []string
	skipped  string
	errored  string
	goErrors []string
	cleanups []func()
	taps     []*Tap
	subs     []*TestResult

	// peerFindingsIgnored suppresses validator findings about frames
	// sent by the peer, for tests whose peer misbehaves on purpose.
	peerFindingsIgnored bool
}

// SubResult records the result of a sub-test, for tests (such as
// h2spec) that run many cases at once. The sub-test's name is
// appended to the test's name.
func (tc *testCtx) SubResult(name string, o Outcome, failures ...string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.subs = append(tc.subs, &TestResult{Name: tc.name + "/" + name, Outcome: o, Failures: failures})
}

func (tc *testCtx) Logf(format string, args ...any) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	fmt.Fprintf(&tc.log, format, args...)
	if !strings.HasSuffix(format, "\n") {
		tc.log.WriteByte('\n')
	}
}

// Failf records an interop or conformance failure.
func (tc *testCtx) Failf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tc.Logf("FAIL: %s", msg)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.failures = append(tc.failures, msg)
}

// Warnf records a non-fatal issue.
func (tc *testCtx) Warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tc.Logf("WARN: %s", msg)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.warnings = append(tc.warnings, msg)
}

// Skipf marks the test as not applicable.
func (tc *testCtx) Skipf(format string, args ...any) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.skipped = fmt.Sprintf(format, args...)
}

// Errorf records a test infrastructure error.
func (tc *testCtx) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tc.Logf("ERROR: %s", msg)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.errored == "" {
		tc.errored = msg
	}
}

// Cleanup registers f to be run when the test finishes, in LIFO order.
func (tc *testCtx) Cleanup(f func()) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.cleanups = append(tc.cleanups, f)
}

// countError records an HTTP/2 error counted by the Go implementation.
func (tc *testCtx) countError(who, token string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.goErrors = append(tc.goErrors, who+": "+token)
}

// Tap starts a tap in front of upstream, closed when the test finishes.
func (tc *testCtx) Tap(upstream string, useTLS bool, clientName, serverName string) (*Tap, error) {
	t, err := startTap(upstream, useTLS, clientName, serverName)
	if err != nil {
		return nil, err
	}
	tc.mu.Lock()
	tc.taps = append(tc.taps, t)
	tc.mu.Unlock()
	tc.Cleanup(t.Close)
	return t, nil
}

func (tc *testCtx) runCleanups() {
	for {
		tc.mu.Lock()
		if len(tc.cleanups) == 0 {
			tc.mu.Unlock()
			return
		}
		f := tc.cleanups[len(tc.cleanups)-1]
		tc.cleanups = tc.cleanups[:len(tc.cleanups)-1]
		tc.mu.Unlock()
		f()
	}
}

// isGoName reports whether an endpoint name refers to the Go implementation.
func isGoName(name string) bool { return strings.HasPrefix(name, "Go ") }

func runTest(ctx context.Context, env *Env, t *Test) []*TestResult {
	start := time.Now()
	tc := &testCtx{name: t.Name, env: env}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = env.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if e := recover(); e != nil {
				tc.Errorf("panic: %v", e)
			}
		}()
		t.Run(ctx, tc)
	}()
	const grace = 30 * time.Second
	select {
	case <-done:
	case <-time.After(timeout + grace):
		// The test (or the implementation under test) ignored
		// cancellation. Abandon it, leaking its goroutines.
		tc.Failf("test hung: still running %v after its %v timeout", grace, timeout)
	}
	if ctx.Err() == context.DeadlineExceeded {
		tc.Failf("test timed out after %v", timeout)
	}
	cancel()
	tc.runCleanups()

	res := &TestResult{
		Name:     t.Name,
		Peer:     t.Peer,
		Duration: time.Since(start),
		GoErrors: tc.goErrors,
	}
	var trace strings.Builder
	for _, tap := range tc.taps {
		for _, v := range tap.Validators() {
			tc.Logf("%s", v.Summary())
			trace.WriteString(v.Trace())
			for _, f := range v.Findings() {
				switch {
				case tc.peerFindingsIgnored && !isGoName(f.Sender):
					continue
				case f.Sev == SevInfo:
					tc.Logf("validator: %v", f)
					continue
				case f.Sev == SevViolation && isGoName(f.Sender):
					tc.failures = append(tc.failures, "spec violation: "+f.String())
				default:
					tc.warnings = append(tc.warnings, f.String())
				}
				res.Findings = append(res.Findings, f)
			}
		}
	}
	res.Failures = tc.failures
	res.Warnings = tc.warnings
	switch {
	case tc.skipped != "" && len(tc.failures) == 0:
		res.Outcome = Skip
		res.Failures = []string{tc.skipped}
	case tc.errored != "":
		res.Outcome = Error
		res.Failures = append([]string{tc.errored}, res.Failures...)
	case len(tc.failures) > 0:
		res.Outcome = Fail
	case len(tc.warnings) > 0:
		res.Outcome = Warn
	default:
		res.Outcome = Pass
	}
	res.Log = tc.log.String()
	res.Trace = trace.String()
	res.Known = env.Known.match(t.Name)
	results := []*TestResult{res}
	for _, sub := range tc.subs {
		sub.Peer = t.Peer
		sub.Known = env.Known.match(sub.Name)
		results = append(results, sub)
	}
	return results
}

// knownEntry is a line from the known-results file.
type knownEntry struct {
	Pattern string  `json:"pattern"`
	Outcome Outcome `json:"outcome"`
	Comment string  `json:"comment,omitempty"`
	re      *regexp.Regexp
}

type knownResults []*knownEntry

// parseKnown parses a known-results file. Each non-blank, non-comment
// line has the form:
//
//	<test name pattern> <PASS|FAIL|FLAKY|ERROR> [# comment]
//
// In patterns, "*" matches any sequence of characters, including "/".
// The last matching line wins.
func parseKnown(data []byte) (knownResults, error) {
	var kr knownResults
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineno := 0
	for sc.Scan() {
		lineno++
		line := sc.Text()
		var comment string
		if i := strings.Index(line, "#"); i >= 0 {
			comment = strings.TrimSpace(line[i+1:])
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("line %d: want \"pattern outcome\"", lineno)
		}
		o := Outcome(f[1])
		if o != Pass && o != Fail && o != Flaky && o != Error {
			return nil, fmt.Errorf("line %d: bad outcome %q", lineno, f[1])
		}
		var re strings.Builder
		re.WriteString("^")
		for i, part := range strings.Split(f[0], "*") {
			if i > 0 {
				re.WriteString(".*")
			}
			re.WriteString(regexp.QuoteMeta(part))
		}
		re.WriteString("$")
		kr = append(kr, &knownEntry{Pattern: f[0], Outcome: o, Comment: comment, re: regexp.MustCompile(re.String())})
	}
	return kr, sc.Err()
}

func (kr knownResults) match(name string) *knownEntry {
	for i := len(kr) - 1; i >= 0; i-- {
		if kr[i].re.MatchString(name) {
			return kr[i]
		}
	}
	return nil
}

// runAll runs tests with the given parallelism and calls report as
// each finishes.
func runAll(ctx context.Context, env *Env, tests []*Test, parallel int, report func(*TestResult)) []*TestResult {
	results := make([][]*TestResult, len(tests))
	var excl, conc []int
	for i, t := range tests {
		if t.Exclusive {
			excl = append(excl, i)
		} else {
			conc = append(conc, i)
		}
	}
	var mu sync.Mutex
	run := func(i int) {
		rs := runTest(ctx, env, tests[i])
		mu.Lock()
		results[i] = rs
		for _, r := range rs {
			report(r)
		}
		mu.Unlock()
	}
	sem := make(chan bool, parallel)
	var wg sync.WaitGroup
	for _, i := range conc {
		if ctx.Err() != nil {
			break
		}
		sem <- true
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			run(i)
		}()
	}
	wg.Wait()
	for _, i := range excl {
		if ctx.Err() != nil {
			break
		}
		run(i)
	}
	var all []*TestResult
	for _, rs := range results {
		all = append(all, rs...)
	}
	return all
}

func readKnownFile(name string) (knownResults, error) {
	if name == "" {
		return parseKnown(knownResultsData)
	}
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseKnown(data)
}
