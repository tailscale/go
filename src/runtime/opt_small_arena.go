// +build runtime_small_arena

package runtime

// want4MB is whether the memory allocator should use a 4MB arena
// instead of the default (64MB on 64-bit non-Windows platforms).
//
// See malloc.go.
const want4MB = 1
