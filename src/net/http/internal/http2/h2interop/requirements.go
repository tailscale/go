// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// This file tracks which tests cover which normative requirements of
// the HTTP/2 RFCs, listed in requirements.txt.
//
// Tests declare the requirements they cover: interop Cases and the
// conformance cases have a covers field, h2spec cases are mapped in
// h2specReqs, and validator checks name the requirement they enforce
// when reporting a finding. Each requirement must be covered, or
// marked n/a, for both roles: the Go client and the Go server.

//go:embed requirements.txt
var requirementsData []byte

// validate.go is scanned for the requirement IDs that validator
// checks report.
//
//go:embed validate.go
var validatorSource []byte

// Requirement is a normative requirement from an RFC.
type Requirement struct {
	ID       string // e.g. "9113-6.5-4"
	Keywords string // e.g. "MUST,MUST_NOT"
	Section  string // e.g. "9113 §6.5"
	Title    string
	Text     string
	NA       map[string]string // role -> reason the role doesn't apply
}

// RFC returns a citation such as "RFC 9113 §6.5".
func (r *Requirement) RFC() string { return "RFC " + r.Section }

var roles = []string{"client", "server"}

var reqIDRE = regexp.MustCompile(`\b(9113|7541|9218|8441)-[0-9A-Z][0-9A-Za-z.]*-d?[0-9]+\b`)

func parseRequirements(data []byte) ([]*Requirement, error) {
	var reqs []*Requirement
	var cur *Requirement
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineno := 0
	for sc.Scan() {
		lineno++
		line := sc.Text()
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case !strings.HasPrefix(line, "\t"):
			// <id> <keywords> §<section> <title>
			f := strings.SplitN(line, " ", 4)
			if len(f) < 3 || !reqIDRE.MatchString(f[0]) || !strings.HasPrefix(f[2], "§") {
				return nil, fmt.Errorf("requirements.txt:%d: bad requirement line %q", lineno, line)
			}
			rfc, _, _ := strings.Cut(f[0], "-")
			cur = &Requirement{ID: f[0], Keywords: f[1], Section: rfc + " " + f[2], NA: map[string]string{}}
			if len(f) == 4 {
				cur.Title = f[3]
			}
			reqs = append(reqs, cur)
		case cur == nil:
			return nil, fmt.Errorf("requirements.txt:%d: text before first requirement", lineno)
		case strings.HasPrefix(line, "\tn/a "):
			role, reason, ok := strings.Cut(strings.TrimPrefix(line, "\tn/a "), ": ")
			if !ok || !slices.Contains(roles, role) || reason == "" {
				return nil, fmt.Errorf("requirements.txt:%d: want \"n/a <client|server>: <reason>\"", lineno)
			}
			cur.NA[role] = reason
		default:
			if cur.Text != "" {
				cur.Text += " "
			}
			cur.Text += strings.TrimSpace(line)
		}
	}
	return reqs, sc.Err()
}

// coverage maps a requirement ID and role to the tests covering it.
type coverage map[string]map[string][]string

func (c coverage) add(id, role, by string) {
	if c[id] == nil {
		c[id] = map[string][]string{}
	}
	if !slices.Contains(c[id][role], by) {
		c[id][role] = append(c[id][role], by)
	}
}

// declaredCoverage returns the coverage declared by tests, and
// any problems with the declarations.
func declaredCoverage() coverage {
	cov := coverage{}
	addAll := func(ids, role, by string) {
		for id := range strings.FieldsSeq(ids) {
			if role == "" {
				for _, r := range roles {
					cov.add(id, r, by)
				}
			} else {
				cov.add(id, role, by)
			}
		}
	}
	for _, c := range cases {
		addAll(c.Covers, "", "interop:"+c.Name)
	}
	for _, c := range clientConfCases {
		addAll(c.covers, "client", "conform/client/"+c.name)
	}
	for _, c := range serverConfCases {
		addAll(c.covers, "server", "conform/server/"+c.name)
	}
	for _, c := range tlsConfCases {
		addAll(c.covers, c.role, "conform/tls/"+c.role+"/"+c.name)
	}
	for name, ids := range h2specReqs {
		addAll(ids, "server", "h2spec:"+name)
	}
	// Validator checks apply to frames sent by both the Go client
	// and the Go server, in every test.
	for _, id := range reqIDRE.FindAllString(string(validatorSource), -1) {
		addAll(id, "", "validator")
	}
	return cov
}

// checkRequirements returns problems with the requirements and their
// coverage: unknown IDs referenced by tests, and roles of requirements
// that are neither covered nor marked n/a.
func checkRequirements(reqs []*Requirement, cov coverage) (unknown []string, uncovered []string) {
	known := map[string]bool{}
	for _, r := range reqs {
		if known[r.ID] {
			unknown = append(unknown, "duplicate requirement "+r.ID)
		}
		known[r.ID] = true
	}
	for _, id := range sortedKeys(cov) {
		if !known[id] {
			var by []string
			for _, role := range roles {
				by = append(by, cov[id][role]...)
			}
			unknown = append(unknown, fmt.Sprintf("%s (referenced by %s)", id, strings.Join(by, ", ")))
		}
	}
	for _, r := range reqs {
		for _, role := range roles {
			if len(cov[r.ID][role]) == 0 && r.NA[role] == "" {
				uncovered = append(uncovered, r.ID+" "+role)
			}
		}
	}
	return unknown, uncovered
}

// writeRequirementsReport writes the coverage of each requirement.
func writeRequirementsReport(w io.Writer, reqs []*Requirement, cov coverage, onlyUncovered bool) {
	nCovered, nNA, nUncovered := 0, 0, 0
	for _, r := range reqs {
		var lines []string
		bad := false
		for _, role := range roles {
			switch by := cov[r.ID][role]; {
			case len(by) > 0:
				nCovered++
				lines = append(lines, fmt.Sprintf("\t%s: %s", role, strings.Join(by, ", ")))
			case r.NA[role] != "":
				nNA++
				lines = append(lines, fmt.Sprintf("\t%s: n/a: %s", role, r.NA[role]))
			default:
				nUncovered++
				bad = true
				lines = append(lines, fmt.Sprintf("\t%s: UNCOVERED", role))
			}
		}
		if onlyUncovered && !bad {
			continue
		}
		fmt.Fprintf(w, "%s %s %s %s\n\t%s\n%s\n\n", r.ID, r.Keywords, r.RFC(), r.Title, r.Text, strings.Join(lines, "\n"))
	}
	fmt.Fprintf(w, "%d requirements x %d roles: %d covered, %d n/a, %d uncovered\n", len(reqs), len(roles), nCovered, nNA, nUncovered)
}

var requirementsByID = func() map[string]*Requirement {
	reqs, err := parseRequirements(requirementsData)
	if err != nil {
		panic(err)
	}
	m := map[string]*Requirement{}
	for _, r := range reqs {
		m[r.ID] = r
	}
	return m
}()

// ruleCitation returns the citation to show for a finding's rule,
// which is a requirement ID or a free-form citation.
func ruleCitation(rule string) string {
	var cites []string
	for id := range strings.FieldsSeq(rule) {
		r := requirementsByID[id]
		if r == nil {
			return rule
		}
		cites = append(cites, r.RFC()+" ("+r.ID+")")
	}
	return strings.Join(cites, ", ")
}
