// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"context"
)

// SockTrace is a set of hooks to run at various operations on a network socket.
// Any particular hook may be nil. Functions may be called concurrently from
// different goroutines.
type SockTrace struct {
	// DidRead is called after a successful read from the socket, where n bytes
	// were read.
	DidRead func(n int)
	// DidWrite is called after a successful write to the socket, where n bytes
	// were written.
	DidWrite func(n int)
	// WillOverwrite is called when the registered trace is overwritten by a
	// subsequent call to WithSockTrace. The provided trace is the new trace
	// that will be used.
	WillOverwrite func(trace *SockTrace)
}

// WithSockTrace returns a new context based on the provided parent
// ctx. Socket reads and writes made with the returned context will use
// the provided trace hooks. Any previous hooks registered with ctx are
// ovewritten (their WillOverwrite hook will be called).
func WithSockTrace(ctx context.Context, trace *SockTrace) context.Context {
	if previous := ContextSockTrace(ctx); previous != nil && previous.WillOverwrite != nil {
		previous.WillOverwrite(trace)
	}
	return context.WithValue(ctx, sockTraceKey{}, trace)
}

// ContextSockTrace returns the SockTrace associated with the
// provided context. If none, it returns nil.
func ContextSockTrace(ctx context.Context) *SockTrace {
	trace, _ := ctx.Value(sockTraceKey{}).(*SockTrace)
	return trace
}

// unique type to prevent assignment.
type sockTraceKey struct{}
