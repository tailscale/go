// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

// summarize writes a summary of results to w and returns the number
// of unexpected results.
func summarize(w io.Writer, results []*TestResult, elapsed time.Duration) int {
	counts := map[Outcome]int{}
	var unexpected []*TestResult
	for _, r := range results {
		counts[r.Outcome]++
		if r.Unexpected() {
			unexpected = append(unexpected, r)
		}
	}
	fmt.Fprintf(w, "\n%d tests in %v: %d passed, %d warned, %d failed, %d errors, %d skipped\n",
		len(results), elapsed.Round(time.Second), counts[Pass], counts[Warn], counts[Fail], counts[Error], counts[Skip])

	if groups := groupFindings(results, true); len(groups) > 0 {
		fmt.Fprintf(w, "\nSpec violations by the Go implementation:\n")
		for _, g := range groups {
			fmt.Fprintf(w, "  %s\n", g)
		}
	}
	if groups := groupFindings(results, false); len(groups) > 0 {
		fmt.Fprintf(w, "\nSpec violations and warnings by peers:\n")
		for _, g := range groups {
			fmt.Fprintf(w, "  %s\n", g)
		}
	}
	if len(unexpected) > 0 {
		fmt.Fprintf(w, "\n%d unexpected results:\n", len(unexpected))
		for _, r := range unexpected {
			want := Pass
			if r.Known != nil {
				want = r.Known.Outcome
			}
			fmt.Fprintf(w, "  %s: got %s, want %s\n", r.Name, r.Outcome, want)
		}
	} else {
		fmt.Fprintf(w, "\nAll results as expected.\n")
	}
	return len(unexpected)
}

var digitsRE = regexp.MustCompile(`[0-9]+`)

type findingGroup struct {
	key    string
	count  int
	tests  []string
	sample string
}

func (g *findingGroup) String() string {
	ex := g.tests
	more := ""
	if len(ex) > 3 {
		more = fmt.Sprintf(", and %d more", len(ex)-3)
		ex = ex[:3]
	}
	return fmt.Sprintf("%s\n      %d occurrences in %d tests (e.g. %s%s)", g.sample, g.count, len(g.tests), strings.Join(ex, ", "), more)
}

// groupFindings groups the validator findings in results, either
// those sent by Go or those sent by peers.
func groupFindings(results []*TestResult, goSide bool) []*findingGroup {
	m := map[string]*findingGroup{}
	for _, r := range results {
		for _, f := range r.Findings {
			if isGoName(f.Sender) != goSide {
				continue
			}
			if goSide && f.Sev != SevViolation {
				continue
			}
			norm := f
			norm.Stream = 0
			norm.Msg = digitsRE.ReplaceAllString(f.Msg, "N")
			key := norm.String()
			g := m[key]
			if g == nil {
				s := f
				s.Stream = 0
				g = &findingGroup{key: key, sample: s.String()}
				m[key] = g
			}
			g.count++
			if !slices.Contains(g.tests, r.Name) {
				g.tests = append(g.tests, r.Name)
			}
		}
	}
	var groups []*findingGroup
	for _, g := range m {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *findingGroup) int { return strings.Compare(a.key, b.key) })
	return groups
}

// writeReport writes a Markdown report of results to w.
func writeReport(w io.Writer, results []*TestResult) {
	fmt.Fprintf(w, "# HTTP/2 interop report\n\n")
	fmt.Fprintf(w, "Generated %s.\n\n", time.Now().UTC().Format(time.RFC3339))

	// Matrix of peer x outcome.
	type row struct{ pass, warn, fail, errs, skip int }
	rows := map[string]*row{}
	for _, r := range results {
		group := testGroup(r.Name)
		rw := rows[group]
		if rw == nil {
			rw = &row{}
			rows[group] = rw
		}
		switch r.Outcome {
		case Pass:
			rw.pass++
		case Warn:
			rw.warn++
		case Fail:
			rw.fail++
		case Error:
			rw.errs++
		case Skip:
			rw.skip++
		}
	}
	fmt.Fprintf(w, "| Group | Pass | Warn | Fail | Error | Skip |\n|---|---|---|---|---|---|\n")
	for _, k := range sortedKeys(rows) {
		rw := rows[k]
		fmt.Fprintf(w, "| %s | %d | %d | %d | %d | %d |\n", k, rw.pass, rw.warn, rw.fail, rw.errs, rw.skip)
	}

	if groups := groupFindings(results, true); len(groups) > 0 {
		fmt.Fprintf(w, "\n## Spec violations by Go\n\n")
		for _, g := range groups {
			fmt.Fprintf(w, "- %s\n", strings.ReplaceAll(g.String(), "\n     ", ""))
		}
	}
	if groups := groupFindings(results, false); len(groups) > 0 {
		fmt.Fprintf(w, "\n## Spec violations and warnings by peers\n\n")
		for _, g := range groups {
			fmt.Fprintf(w, "- %s\n", strings.ReplaceAll(g.String(), "\n     ", ""))
		}
	}

	fmt.Fprintf(w, "\n## Failures\n\n")
	for _, r := range results {
		if r.Outcome != Fail && r.Outcome != Error {
			continue
		}
		known := ""
		if r.Known != nil && r.Known.Outcome == r.Outcome {
			known = " (known: " + r.Known.Comment + ")"
		}
		fmt.Fprintf(w, "### %s: %s%s\n\n", r.Name, r.Outcome, known)
		for _, f := range r.Failures {
			fmt.Fprintf(w, "- %s\n", f)
		}
		if len(r.GoErrors) > 0 {
			fmt.Fprintf(w, "- Go CountError: %s\n", strings.Join(r.GoErrors, ", "))
		}
		fmt.Fprintln(w)
	}
}

// testGroup returns the group of a test name for the report matrix:
// its name up to the mode or suite-specific test component.
func testGroup(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "tls" || p == "h2c" {
			return strings.Join(parts[:i], "/")
		}
	}
	if len(parts) > 2 {
		return strings.Join(parts[:2], "/")
	}
	return parts[0]
}
