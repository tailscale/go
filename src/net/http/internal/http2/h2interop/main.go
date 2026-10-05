// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

// H2interop runs an HTTP/2 interoperability and conformance test
// suite against Go's HTTP/2 client (net/http.Transport) and server
// (net/http.Server), as implemented by net/http/internal/http2.
//
// Usage:
//
//	go run -tags=h2interop net/http/internal/http2/h2interop [flags]
//
// The suite has these parts:
//
//   - goserver/<peer>/<mode>/<case>: peer clients make requests to
//     the Go server. The peers are curl, nghttp, the Python h2
//     library, Node.js, the Rust h2 crate, and Java's HttpClient, and
//     the HTTP/2 upstream (client) side of the nginx, HAProxy, Envoy,
//     Apache httpd, and nghttpx reverse proxies.
//   - goclient/<peer>/<mode>/<case>: the Go client makes requests to
//     peer servers: nghttpd, Python h2, Node.js, Rust h2, Jetty, and
//     the reverse proxies' HTTP/2 frontends (in front of a Go HTTP/1
//     backend).
//   - gogo/<mode>/<case>: the Go client makes the same requests to the
//     Go server, as a baseline.
//   - goserver/h2load/...: load tests of the Go server with h2load.
//   - h2spec/<mode>/...: the h2spec conformance suite (RFC 7540 and
//     RFC 7541) against the Go server.
//   - conform/client/... and conform/server/...: scripted frame-level
//     conformance tests of the Go client and server against the
//     requirements of RFC 9113, RFC 7541, RFC 9218, and RFC 8441.
//
// Modes are "tls" (TLS with ALPN) and "h2c" (cleartext HTTP/2 with
// prior knowledge). Peer names and the Go side may carry a settings
// profile suffix, such as "goserver:constrained" or
// "nghttpd:constrained", for configurations with small limits.
//
// Peers run in Docker containers, built from the Dockerfiles in the
// peers directory and run with host networking; see peers/README.md
// for the protocol spoken by library-based peers and the route set
// that servers implement. Every connection passes through an
// in-process tap that validates each frame sent by either endpoint
// against RFC 9113 and RFC 7541 (see validate.go). Spec violations by
// the Go implementation fail the test; violations by peers are
// reported as warnings. HTTP/2 errors counted by the Go implementation
// (via HTTP2Config.CountError) are included in failure reports.
//
// Expected results are recorded in known.txt. The program exits
// non-zero only if a result differs from what known.txt anticipates.
// Use -known=/dev/null to report all failures.
//
// To add an interop scenario, add a Case to cases.go. To add a peer,
// add a directory to peers implementing the JSON client and server
// protocols and register it in peers.go, or write a ClientImpl or
// ServerImpl for a command-line tool (see peer_cli.go and
// peer_proxy.go).
//
// A full run takes a few minutes once the peer images are built.
// Building them the first time takes longer and requires network
// access.
//
// Flags:
//
//	-run regexp    run only tests whose names match regexp
//	-skip regexp   skip tests whose names match regexp
//	-list          list tests and exit
//	-j n           run n tests in parallel
//	-v             print logs of all tests, not just failures
//	-out dir       write results.json, report.md, and per-test logs and frame traces to dir
//	-known file    known-results file (default: the embedded known.txt)
//	-timeout d     per-test timeout
//	-serve mode    serve the route set with the Go server ("tls" or "h2c") on -addr, for debugging peers
//	-requirements all|uncovered
//	               report which tests cover each requirement in requirements.txt
package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http/internal/testcert"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed known.txt
var knownResultsData []byte

var testCert = func() tls.Certificate {
	c, err := tls.X509KeyPair(testcert.LocalhostCert, testcert.LocalhostKey)
	if err != nil {
		panic(err)
	}
	return c
}()

// Env is the environment shared by all tests.
type Env struct {
	Docker  *dockerEnv
	Known   knownResults
	Timeout time.Duration
	Verbose bool
}

var (
	flagRun     = flag.String("run", "", "run only tests whose names match `regexp`")
	flagSkip    = flag.String("skip", "", "skip tests whose names match `regexp`")
	flagList    = flag.Bool("list", false, "list tests and exit")
	flagJ       = flag.Int("j", 8, "number of tests to run in parallel")
	flagV       = flag.Bool("v", false, "print logs of all tests")
	flagOut     = flag.String("out", "", "write results, report, logs and frame traces to `dir`")
	flagKnown   = flag.String("known", "", "known-results `file` (default: embedded known.txt)")
	flagTimeout = flag.Duration("timeout", 60*time.Second, "per-test timeout")
	flagSkips   = flag.Bool("show-skips", false, "list skipped (not applicable) tests in output")
	flagServe   = flag.String("serve", "", "instead of running tests, serve the route set with the Go server in `mode` (tls or h2c) on -addr, for debugging")
	flagAddr    = flag.String("addr", "127.0.0.1:8443", "address for -serve")
	flagReqs    = flag.String("requirements", "", "instead of running tests, report test coverage of RFC requirements: \"all\" or \"uncovered\"")
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("h2interop: ")
	flag.Parse()

	if *flagServe != "" {
		serveForDebugging(*flagServe, *flagAddr)
		return
	}
	if *flagReqs != "" {
		reqs, err := parseRequirements(requirementsData)
		if err != nil {
			log.Fatal(err)
		}
		cov := declaredCoverage()
		writeRequirementsReport(os.Stdout, reqs, cov, *flagReqs == "uncovered")
		if unknown, _ := checkRequirements(reqs, cov); len(unknown) > 0 {
			log.Fatalf("unknown requirement IDs:\n\t%s", strings.Join(unknown, "\n\t"))
		}
		return
	}

	known, err := readKnownFile(*flagKnown)
	if err != nil {
		log.Fatalf("reading known results: %v", err)
	}
	env := &Env{
		Known:   known,
		Timeout: *flagTimeout,
		Verbose: *flagV,
	}

	tests := allTests(*flagSkips)
	tests, err = filterTests(tests, *flagRun, *flagSkip)
	if err != nil {
		log.Fatal(err)
	}
	if *flagList {
		for _, t := range tests {
			fmt.Println(t.Name)
		}
		return
	}
	if len(tests) == 0 {
		log.Fatal("no tests to run")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	needDocker := false
	for _, t := range tests {
		if len(t.Images) > 0 {
			needDocker = true
		}
	}
	if needDocker {
		env.Docker, err = newDockerEnv(*flagV)
		if err != nil {
			log.Fatal(err)
		}
		defer env.Docker.Cleanup()
		if err := env.Docker.BuildImages(ctx, tests); err != nil {
			env.Docker.Cleanup()
			log.Fatal(err)
		}
	}

	if *flagOut != "" {
		if err := os.MkdirAll(filepath.Join(*flagOut, "logs"), 0o755); err != nil {
			log.Fatal(err)
		}
	}

	start := time.Now()
	results := runAll(ctx, env, tests, *flagJ, func(r *TestResult) {
		printResult(r)
		if *flagOut != "" {
			writeTestLog(*flagOut, r)
		}
	})
	unexpected := summarize(os.Stdout, results, time.Since(start))
	if *flagOut != "" {
		j, _ := json.MarshalIndent(results, "", "\t")
		os.WriteFile(filepath.Join(*flagOut, "results.json"), j, 0o644)
		f, err := os.Create(filepath.Join(*flagOut, "report.md"))
		if err == nil {
			writeReport(f, results)
			f.Close()
		}
	}
	if ctx.Err() != nil {
		os.Exit(130)
	}
	if unexpected > 0 {
		os.Exit(1)
	}
}

// allTests returns every test in the suite.
func allTests(includeSkips bool) []*Test {
	var clients []ClientImpl
	var servers []ServerImpl
	for _, p := range goProfiles {
		clients = append(clients, &goClient{profile: p})
		servers = append(servers, &goServer{profile: p})
	}
	clients = append(clients, peerClients...)
	servers = append(servers, peerServers...)
	var tests []*Test
	tests = append(tests, interopTests(clients, servers, includeSkips)...)
	tests = append(tests, extraTests...)
	return tests
}

// extraTests are tests other than interop cases, registered by init
// functions in other files.
var extraTests []*Test

func filterTests(tests []*Test, run, skip string) ([]*Test, error) {
	var runRE, skipRE *regexp.Regexp
	var err error
	if run != "" {
		if runRE, err = regexp.Compile(run); err != nil {
			return nil, err
		}
	}
	if skip != "" {
		if skipRE, err = regexp.Compile(skip); err != nil {
			return nil, err
		}
	}
	var out []*Test
	for _, t := range tests {
		if runRE != nil && !runRE.MatchString(t.Name) {
			continue
		}
		if skipRE != nil && skipRE.MatchString(t.Name) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func printResult(r *TestResult) {
	if r.Outcome == Skip && !*flagSkips {
		return
	}
	mark := ""
	if r.Unexpected() {
		mark = " (unexpected)"
	} else if r.Known != nil && r.Known.Outcome != Pass && r.Outcome != Pass && r.Outcome != Warn {
		mark = " (known)"
	}
	fmt.Printf("%-5s %s (%.1fs)%s\n", r.Outcome, r.Name, r.Duration.Seconds(), mark)
	if r.Outcome == Pass && !*flagV {
		return
	}
	if r.Outcome == Skip {
		fmt.Printf("\t%s\n", r.Failures[0])
		return
	}
	for _, f := range r.Failures {
		fmt.Printf("\t%s\n", indent(f))
	}
	if r.Outcome == Fail || r.Outcome == Error || *flagV {
		for _, w := range r.Warnings {
			fmt.Printf("\twarning: %s\n", indent(w))
		}
		if len(r.GoErrors) > 0 {
			fmt.Printf("\tGo CountError: %s\n", strings.Join(r.GoErrors, ", "))
		}
	} else if len(r.Warnings) > 0 {
		for i, w := range r.Warnings {
			if i == 3 {
				fmt.Printf("\t(%d more warnings)\n", len(r.Warnings)-3)
				break
			}
			fmt.Printf("\twarning: %s\n", indent(w))
		}
	}
	if *flagV {
		fmt.Printf("\t--- log:\n\t%s\n", indent(r.Log))
	}
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n\t")
}

func writeTestLog(dir string, r *TestResult) {
	if r.Outcome == Skip {
		return
	}
	base := filepath.Join(dir, "logs", strings.NewReplacer("/", "__", ":", "_").Replace(r.Name))
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s (%v)\n", r.Name, r.Outcome, r.Duration)
	for _, f := range r.Failures {
		fmt.Fprintf(&sb, "failure: %s\n", f)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&sb, "warning: %s\n", w)
	}
	for _, e := range r.GoErrors {
		fmt.Fprintf(&sb, "Go CountError: %s\n", e)
	}
	sb.WriteString("\n")
	sb.WriteString(r.Log)
	os.WriteFile(base+".log", []byte(sb.String()), 0o644)
	if r.Trace != "" {
		os.WriteFile(base+".frames", []byte(r.Trace), 0o644)
	}
}
