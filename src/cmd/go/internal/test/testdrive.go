// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bufio"
	"bytes"
	"cmd/go/internal/base"
	"cmd/go/internal/cfg"
	"cmd/go/internal/load"
	"cmd/go/internal/str"
	"go/build/testdriveprog"
	"cmd/go/internal/work"
	"cmd/internal/quoted"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// testDriveCtl is the active GOTESTDRIVEPROG client, or nil if not set.
// It is initialized in runTest and used by runTestActor.Act.
var testDriveCtl *testDriveClient

// testDriveClient manages the connection to a GOTESTDRIVEPROG child process.
// Its structure mirrors cache.ProgCache.
type testDriveClient struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser  // from the child process
	stdin  io.WriteCloser // to the child process
	bw     *bufio.Writer  // to stdin
	jenc   *json.Encoder  // to bw

	can map[testdriveprog.Cmd]bool // commands the child supports

	closing      atomic.Bool
	ctx          context.Context
	ctxCancel    context.CancelFunc
	readLoopDone chan struct{}

	mu       sync.Mutex // guards nextID, inFlight
	nextID   int64
	inFlight map[int64]chan<- *testdriveprog.Response

	// writeMu serializes writing to the child process.
	// It must never be held at the same time as mu.
	writeMu sync.Mutex
}

var errTestDriveClosed = errors.New("GOTESTDRIVEPROG closed unexpectedly")

// startTestDriveProg starts the GOTESTDRIVEPROG child process and waits
// for it to advertise its capabilities.
func startTestDriveProg(progAndArgs string) (*testDriveClient, error) {
	args, err := quoted.Split(progAndArgs)
	if err != nil {
		return nil, fmt.Errorf("GOTESTDRIVEPROG args: %v", err)
	}
	var prog string
	if len(args) > 0 {
		prog = args[0]
		args = args[1:]
	}

	ctx, ctxCancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, prog, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("StdoutPipe to GOTESTDRIVEPROG: %v", err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("StdinPipe to GOTESTDRIVEPROG: %v", err)
	}
	cmd.Stderr = os.Stderr
	cmd.Cancel = in.Close

	if err := cmd.Start(); err != nil {
		ctxCancel()
		return nil, fmt.Errorf("starting GOTESTDRIVEPROG %q: %v", prog, err)
	}

	c := &testDriveClient{
		ctx:          ctx,
		ctxCancel:    ctxCancel,
		cmd:          cmd,
		stdout:       out,
		stdin:        in,
		bw:           bufio.NewWriter(in),
		inFlight:     make(map[int64]chan<- *testdriveprog.Response),
		readLoopDone: make(chan struct{}),
	}

	// Register interest in the initial capability message (ID=0).
	capResc := make(chan *testdriveprog.Response, 1)
	c.inFlight[0] = capResc

	c.jenc = json.NewEncoder(c.bw)
	go c.readLoop(c.readLoopDone)

	// Wait for the child to report capabilities.
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			log.Printf("# still waiting for GOTESTDRIVEPROG %v ...", prog)
		case capRes := <-capResc:
			if capRes == nil {
				return nil, fmt.Errorf("GOTESTDRIVEPROG %v exited before sending capabilities", prog)
			}
			can := map[testdriveprog.Cmd]bool{}
			for _, cmd := range capRes.KnownCommands {
				can[cmd] = true
			}
			if len(can) == 0 {
				return nil, fmt.Errorf("GOTESTDRIVEPROG %v declared no supported commands", prog)
			}
			c.can = can
			return c, nil
		}
	}
}

func (c *testDriveClient) readLoop(readLoopDone chan<- struct{}) {
	defer close(readLoopDone)
	jd := json.NewDecoder(c.stdout)
	for {
		res := new(testdriveprog.Response)
		if err := jd.Decode(res); err != nil {
			if c.closing.Load() {
				c.mu.Lock()
				for _, ch := range c.inFlight {
					close(ch)
				}
				c.inFlight = nil
				c.mu.Unlock()
				return
			}
			if err == io.EOF {
				c.mu.Lock()
				inFlight := len(c.inFlight)
				c.mu.Unlock()
				base.Fatalf("GOTESTDRIVEPROG exited pre-Close with %v pending requests", inFlight)
			}
			base.Fatalf("error reading JSON from GOTESTDRIVEPROG: %v", err)
		}
		c.mu.Lock()
		ch, ok := c.inFlight[res.ID]
		delete(c.inFlight, res.ID)
		c.mu.Unlock()
		if ok {
			ch <- res
		} else {
			base.Fatalf("GOTESTDRIVEPROG sent response for unknown request ID %v", res.ID)
		}
	}
}

func (c *testDriveClient) send(ctx context.Context, req *testdriveprog.Request) (*testdriveprog.Response, error) {
	resc := make(chan *testdriveprog.Response, 1)
	if err := c.writeToChild(req, resc); err != nil {
		return nil, err
	}
	select {
	case res := <-resc:
		if res == nil {
			return nil, errTestDriveClosed
		}
		if res.Error != "" {
			return nil, errors.New(res.Error)
		}
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *testDriveClient) writeToChild(req *testdriveprog.Request, resc chan<- *testdriveprog.Response) (err error) {
	c.mu.Lock()
	if c.inFlight == nil {
		c.mu.Unlock()
		return errTestDriveClosed
	}
	c.nextID++
	req.ID = c.nextID
	c.inFlight[req.ID] = resc
	c.mu.Unlock()

	defer func() {
		if err != nil {
			c.mu.Lock()
			if c.inFlight != nil {
				delete(c.inFlight, req.ID)
			}
			c.mu.Unlock()
		}
	}()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.jenc.Encode(req); err != nil {
		return err
	}
	if err := c.bw.WriteByte('\n'); err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}
	return nil
}

// Close sends the close command and shuts down the child process.
func (c *testDriveClient) Close() error {
	c.closing.Store(true)
	var err error
	if c.can[testdriveprog.CmdClose] {
		_, err = c.send(c.ctx, &testdriveprog.Request{Command: testdriveprog.CmdClose})
		if errors.Is(err, errTestDriveClosed) {
			err = nil
		}
	}
	c.ctxCancel()
	<-c.readLoopDone
	return err
}

// Discover sends the list of discovered packages to the child and
// filters out any packages the child says to skip.
func (c *testDriveClient) Discover(ctx context.Context, pkgs []*load.Package) []*load.Package {
	if !c.can[testdriveprog.CmdDiscover] {
		return pkgs
	}

	infos := make([]testdriveprog.PackageInfo, len(pkgs))
	for i, p := range pkgs {
		infos[i] = testdriveprog.PackageInfo{
			Path:     p.ImportPath,
			HasTests: len(p.TestGoFiles)+len(p.XTestGoFiles) > 0,
		}
	}

	res, err := c.send(ctx, &testdriveprog.Request{
		Command:  testdriveprog.CmdDiscover,
		Packages: infos,
	})
	if err != nil {
		base.Fatalf("GOTESTDRIVEPROG discover: %v", err)
	}

	if len(res.SkipPackages) == 0 {
		return pkgs
	}

	skip := make(map[string]bool, len(res.SkipPackages))
	for _, p := range res.SkipPackages {
		skip[p] = true
	}
	filtered := make([]*load.Package, 0, len(pkgs))
	for _, p := range pkgs {
		if !skip[p.ImportPath] {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// shouldStartPackage asks the child whether to proceed with building and
// testing the given package. It reports whether the package should be started.
func (c *testDriveClient) shouldStartPackage(ctx context.Context, pkgPath string) bool {
	if !c.can[testdriveprog.CmdStartPackage] {
		return true
	}
	res, err := c.send(ctx, &testdriveprog.Request{
		Command:     testdriveprog.CmdStartPackage,
		PackagePath: pkgPath,
	})
	if err != nil {
		base.Fatalf("GOTESTDRIVEPROG start_package %s: %v", pkgPath, err)
	}
	return !res.Skip
}

// runWithTestDrive handles the test execution lifecycle for a single
// package when GOTESTDRIVEPROG is active. It replaces the normal
// test execution path in runTestActor.Act.
func runWithTestDrive(ctx context.Context, b *work.Builder, a *work.Action, stdout io.Writer) error {
	c := testDriveCtl
	p := a.Package
	pkgPath := p.ImportPath

	barrierAction := a.Deps[0]
	buildAction := barrierAction.Deps[0]

	// Handle build failure.
	if a.Failed != nil {
		if c.can[testdriveprog.CmdBuildResult] {
			errMsg := "build failed"
			if a.Failed.Package != nil {
				errMsg = fmt.Sprintf("build failed: %s", a.Failed.Package.Desc())
			}
			c.send(ctx, &testdriveprog.Request{
				Command:     testdriveprog.CmdBuildResult,
				PackagePath: pkgPath,
				BuildError:  errMsg,
			})
		}
		a.Failed = nil
		fmt.Fprintf(stdout, "FAIL\t%s [build failed]\n", pkgPath)
		base.SetExitStatus(1)
		return nil
	}

	// Handle packages with no test files.
	if len(p.TestGoFiles)+len(p.XTestGoFiles) == 0 {
		fmt.Fprintf(stdout, "?\t%s\t[no test files]\n", pkgPath)
		return nil
	}

	// Compute binary hash.
	binaryPath := buildAction.BuiltTarget()
	binHash, err := computeBinaryHash(binaryPath)
	if err != nil {
		return fmt.Errorf("hashing test binary for %s: %v", pkgPath, err)
	}

	// Send build result.
	if c.can[testdriveprog.CmdBuildResult] {
		res, err := c.send(ctx, &testdriveprog.Request{
			Command:     testdriveprog.CmdBuildResult,
			PackagePath: pkgPath,
			BinaryHash:  binHash,
		})
		if err != nil {
			return fmt.Errorf("GOTESTDRIVEPROG build_result %s: %v", pkgPath, err)
		}
		if res.SkipTests {
			fmt.Fprintf(stdout, "ok  \t%s\t(cached by testdriveprog)\n", pkgPath)
			return nil
		}
	}

	// Run -test.list to discover test names.
	testNames, err := listTests(ctx, binaryPath, p.Dir)
	if err != nil {
		return fmt.Errorf("listing tests for %s: %v", pkgPath, err)
	}

	// Send test list and get initial assignment.
	var runTests []string
	var packageDone bool

	if c.can[testdriveprog.CmdTestList] {
		res, err := c.send(ctx, &testdriveprog.Request{
			Command:     testdriveprog.CmdTestList,
			PackagePath: pkgPath,
			BinaryHash:  binHash,
			Tests:       testNames,
		})
		if err != nil {
			return fmt.Errorf("GOTESTDRIVEPROG test_list %s: %v", pkgPath, err)
		}
		runTests = res.RunTests
		packageDone = res.PackageDone
	} else {
		// If child doesn't support test_list, run all tests.
		runTests = testNames
		packageDone = true
	}

	// Prepare test args: strip -test.run and -test.list from user args.
	baseTestArgs := filterTestArgs(testArgs)
	// Always add -test.v for result parsing.
	hasV := false
	for _, arg := range baseTestArgs {
		if arg == "-test.v" || arg == "-test.v=true" {
			hasV = true
			break
		}
	}
	if !hasV {
		baseTestArgs = append([]string{"-test.v"}, baseTestArgs...)
	}

	t0 := time.Now()
	anyFailed := false

	// Main execution loop: run batches of tests as assigned by the child.
	for {
		if len(runTests) == 0 {
			if packageDone {
				break
			}
			// No tests to run right now; ask for more.
			if !c.can[testdriveprog.CmdTestAwait] {
				break
			}
			res, err := c.send(ctx, &testdriveprog.Request{
				Command:     testdriveprog.CmdTestAwait,
				PackagePath: pkgPath,
			})
			if err != nil {
				return fmt.Errorf("GOTESTDRIVEPROG test_await %s: %v", pkgPath, err)
			}
			runTests = res.RunTests
			packageDone = res.PackageDone
			continue
		}

		// Run this batch of tests.
		runRegex := buildRunRegex(runTests)
		args := str.StringList(work.FindExecCmd(), binaryPath, "-test.run="+runRegex, baseTestArgs)

		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = p.Dir
		cmd.Env = append(cfg.OrigEnv[:len(cfg.OrigEnv):len(cfg.OrigEnv)], base.AppendPATH(nil)...)
		cmd.Env = base.AppendPWD(cmd.Env, cmd.Dir)
		// Write output both to our buffer (for parsing) and to stdout (for the user).
		cmd.Stdout = io.MultiWriter(&buf, stdout)
		cmd.Stderr = io.MultiWriter(&buf, stdout)

		cmdErr := cmd.Run()

		// Parse results from verbose output.
		results := parseTestResults(buf.Bytes())

		// Clear runTests for this batch; we'll accumulate new assignments from responses.
		runTests = nil

		// Report each test result.
		for _, tr := range results {
			if c.can[testdriveprog.CmdTestResult] {
				res, err := c.send(ctx, &testdriveprog.Request{
					Command:     testdriveprog.CmdTestResult,
					PackagePath: pkgPath,
					TestName:    tr.name,
					Passed:      tr.passed,
					Skipped:     tr.skipped,
					Output:      tr.output,
					Elapsed:     tr.elapsed,
				})
				if err != nil {
					return fmt.Errorf("GOTESTDRIVEPROG test_result %s/%s: %v", pkgPath, tr.name, err)
				}
				// Accumulate any new test assignments from the response.
				runTests = append(runTests, res.RunTests...)
				if res.PackageDone {
					packageDone = true
				}
			}

			if !tr.passed && !tr.skipped {
				anyFailed = true
			}
		}

		// If the binary exited non-zero but we found no individual results,
		// report the overall failure.
		if cmdErr != nil && len(results) == 0 {
			anyFailed = true
		}
	}

	// Print summary line.
	elapsed := fmt.Sprintf("%.3fs", time.Since(t0).Seconds())
	if anyFailed {
		fmt.Fprintf(stdout, "FAIL\t%s\t%s\n", pkgPath, elapsed)
		base.SetExitStatus(1)
	} else {
		fmt.Fprintf(stdout, "ok  \t%s\t%s\n", pkgPath, elapsed)
	}

	return nil
}

// computeBinaryHash returns the hex-encoded SHA256 of the file at path.
func computeBinaryHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// listTests runs the test binary with -test.list . to discover all test
// function names.
func listTests(ctx context.Context, binaryPath, dir string) ([]string, error) {
	args := str.StringList(work.FindExecCmd(), binaryPath, "-test.list", ".")
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(cfg.OrigEnv[:len(cfg.OrigEnv):len(cfg.OrigEnv)], base.AppendPATH(nil)...)
	cmd.Env = base.AppendPWD(cmd.Env, cmd.Dir)

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running -test.list: %v", err)
	}

	var names []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			names = append(names, line)
		}
	}
	return names, scanner.Err()
}

// buildRunRegex constructs a -test.run regex that matches exactly the
// given test names.
func buildRunRegex(tests []string) string {
	parts := make([]string, len(tests))
	for i, t := range tests {
		parts[i] = regexp.QuoteMeta(t)
	}
	return "^(" + strings.Join(parts, "|") + ")$"
}

// filterTestArgs returns a copy of args with -test.run and -test.list
// flags removed, since the testdrive client controls which tests run.
func filterTestArgs(args []string) []string {
	var filtered []string
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(arg, "-test.run=") || strings.HasPrefix(arg, "-test.list=") {
			continue
		}
		if arg == "-test.run" || arg == "-test.list" {
			skipNext = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

// testResult holds the parsed result of a single test from verbose output.
type testResult struct {
	name    string
	passed  bool
	skipped bool
	elapsed float64
	output  string
}

// testResultLineRe matches --- PASS/FAIL/SKIP lines in -test.v output.
// Only matches top-level test results (no leading whitespace).
var testResultLineRe = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (.+?) \((\d+(?:\.\d+)?)s\)`)

// parseTestResults extracts per-test results from -test.v output.
func parseTestResults(output []byte) []testResult {
	var results []testResult
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		m := testResultLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		status, name, durStr := m[1], m[2], m[3]
		// Skip subtests (they contain "/").
		if strings.Contains(name, "/") {
			continue
		}
		tr := testResult{
			name: name,
		}
		switch status {
		case "PASS":
			tr.passed = true
		case "SKIP":
			tr.skipped = true
		}
		fmt.Sscanf(durStr, "%f", &tr.elapsed)
		results = append(results, tr)
	}
	return results
}
