// +build windows

package runtime

import (
	"unsafe"
)

func allocAndCopyString(data *byte, n int) (str string) {
	p := winAlloc(uintptr(n))
	stringStructOf(&str).str = p
	stringStructOf(&str).len = n
	memmove(p, unsafe.Pointer(data), uintptr(n))
	winProtect(p, uintptr(n))
	return
}

//go:nosplit
func winAlloc(sz uintptr) unsafe.Pointer {
	return unsafe.Pointer(stdcall4(_VirtualAlloc, 0, sz, _MEM_COMMIT|_MEM_RESERVE, _PAGE_READWRITE))
}

//go:nosplit
func winProtect(ptr unsafe.Pointer, sz uintptr) {
	var old uint32
	stdcall4(_VirtualProtect, uintptr(ptr), sz, _MEM_COMMIT|_MEM_RESERVE, _PAGE_READONLY)
	if old != _PAGE_READWRITE {
		throw("double protect?")
	}
}
