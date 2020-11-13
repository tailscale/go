// +build !windows

package runtime

import "unsafe"

func allocAndCopyString(data *byte, n int) (str string) {
	p := mallocgc(uintptr(n), nil, false)
	stringStructOf(&str).str = p
	stringStructOf(&str).len = n
	memmove(p, unsafe.Pointer(data), uintptr(n))
	return
}
