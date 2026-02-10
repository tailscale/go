// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package testdriveprog defines the protocol between the go command
// and a GOTESTDRIVEPROG child process.
//
// The GOTESTDRIVEPROG environment variable names a program to run
// that coordinates distributed test execution. The go command communicates
// with the child process over JSON messages on stdin/stdout, similar to
// the GOCACHEPROG protocol.
//
// When GOTESTDRIVEPROG is set, the go command notifies the child about
// discovered packages, asks permission before building and running tests,
// shares test binary identity and discovered test function names, and
// reports per-test results. The child process can choose which packages
// and tests to run on this machine, enabling multiple machines to
// coordinate a single "go test ./..." without duplicating work.
//
// # Protocol
//
// The child process must write a single JSON Response with ID=0 and
// KnownCommands populated at startup. After that, the go command sends
// Request messages with auto-incrementing IDs, and the child sends
// Response messages with matching IDs. Responses may arrive out of order.
//
// The child can piggyback test assignments (RunTests) on any response
// to test_list, test_result, or test_await commands.
package testdriveprog

// Cmd is a command string used in the protocol.
type Cmd string

const (
	// CmdDiscover is sent after package discovery. The request includes
	// the list of all packages found, with metadata about whether each
	// has test files. The response may include packages to skip.
	CmdDiscover = Cmd("discover")

	// CmdStartPackage is sent before building a test binary for a package.
	// The child can respond with Skip=true to skip this package entirely.
	CmdStartPackage = Cmd("start_package")

	// CmdBuildResult is sent after the test binary build completes.
	// On success, BinaryHash contains the hex-encoded SHA256 of the
	// linked test binary. On failure, BuildError contains the error message.
	// The child may respond with SkipTests=true if it knows the binary
	// has already passed all tests.
	CmdBuildResult = Cmd("build_result")

	// CmdTestList is sent after running the test binary with -test.list
	// to discover test function names. The request includes the test names
	// and BinaryHash. The child responds with which tests to run.
	CmdTestList = Cmd("test_list")

	// CmdTestResult is sent after a test completes. The child may
	// piggyback additional RunTests in its response.
	CmdTestResult = Cmd("test_result")

	// CmdTestAwait is sent when the parent has no more tests to run for
	// a package but PackageDone has not been received. The child can
	// respond with more RunTests or signal PackageDone.
	CmdTestAwait = Cmd("test_await")

	// CmdClose requests graceful shutdown. The child should respond
	// and then exit.
	CmdClose = Cmd("close")
)

// Request is a message from the go command to the GOTESTDRIVEPROG child.
type Request struct {
	// ID is a unique identifier for this request, assigned by the parent.
	// The child must echo it in the corresponding Response.
	ID      int64 `json:"id"`
	Command Cmd   `json:"command"`

	// Packages is set for CmdDiscover requests.
	Packages []PackageInfo `json:"packages,omitempty"`

	// PackagePath identifies the package for CmdStartPackage,
	// CmdBuildResult, CmdTestList, CmdTestResult, and CmdTestAwait.
	PackagePath string `json:"packagePath,omitempty"`

	// BinaryHash is the hex-encoded SHA256 of the linked test binary.
	// Set for CmdBuildResult (on success) and CmdTestList.
	BinaryHash string `json:"binaryHash,omitempty"`

	// BuildError is non-empty for CmdBuildResult when the build failed.
	BuildError string `json:"buildError,omitempty"`

	// Tests is the list of test function names discovered by -test.list.
	// Set for CmdTestList.
	Tests []string `json:"tests,omitempty"`

	// TestName is the name of the completed test. Set for CmdTestResult.
	TestName string `json:"testName,omitempty"`

	// Passed indicates the test passed. Set for CmdTestResult.
	Passed bool `json:"passed,omitempty"`

	// Skipped indicates the test was skipped. Set for CmdTestResult.
	Skipped bool `json:"skipped,omitempty"`

	// Output is the test's output text. Set for CmdTestResult.
	Output string `json:"output,omitempty"`

	// Elapsed is the test duration in seconds. Set for CmdTestResult.
	Elapsed float64 `json:"elapsed,omitempty"`
}

// PackageInfo describes a discovered package.
type PackageInfo struct {
	// Path is the package import path.
	Path string `json:"path"`
	// HasTests is true if the package has any test files.
	HasTests bool `json:"hasTests"`
}

// Response is a message from the GOTESTDRIVEPROG child to the go command.
type Response struct {
	// ID must match the Request ID, or be 0 for the initial
	// capability advertisement at startup.
	ID int64 `json:"id"`

	// Error, if non-empty, indicates the request failed.
	Error string `json:"error,omitempty"`

	// KnownCommands is set only in the initial response (ID=0)
	// to advertise which commands the child supports.
	KnownCommands []Cmd `json:"knownCommands,omitempty"`

	// SkipPackages is set in the CmdDiscover response to list
	// package paths that should not be tested on this machine.
	// An empty list means proceed with all packages.
	SkipPackages []string `json:"skipPackages,omitempty"`

	// Skip is set in the CmdStartPackage response to indicate
	// the package should be skipped entirely.
	Skip bool `json:"skip,omitempty"`

	// SkipTests is set in the CmdBuildResult response to indicate
	// that all tests should be skipped (e.g., the binary is known
	// to have already passed all tests).
	SkipTests bool `json:"skipTests,omitempty"`

	// RunTests lists test function names that the parent should run.
	// May be set in responses to CmdTestList, CmdTestResult, or CmdTestAwait.
	RunTests []string `json:"runTests,omitempty"`

	// PackageDone indicates that no more tests will be assigned for
	// this package. The parent can move on.
	// May be set in responses to CmdTestList, CmdTestResult, or CmdTestAwait.
	PackageDone bool `json:"packageDone,omitempty"`
}
