package reflect

import (
	"internal/race"
	"sync"
	"unsafe"
)

func Freeze(x interface{}) {
	if !race.Enabled {
		return
	}
	v := ValueOf(x)
	if v.Kind() != Ptr {
		panic("cannot freeze non-pointers")
	}
	var wg sync.WaitGroupNoRace
	wg.Add(1)
	go func() {
		defer wg.Done()
		touch(v)
	}()
	wg.Wait()
}

// touch reads v and writes it back out unchanged.
// TODO: handle cycles
func touch(v Value) {
	if !v.IsValid() || v.typ.size != ptrSize || !v.typ.pointers() {
		return
	}

	if race.Enabled {
		var ptr unsafe.Pointer
		if v.flag&flagIndir != 0 {
			ptr = *(*unsafe.Pointer)(v.ptr)
		} else {
			ptr = v.ptr
		}
		race.WriteRange(ptr, int(v.typ.size))
	}

	// recurse into things v points to
	switch v.Kind() {
	default:
		panic("touch: unhandled kind: " + v.Kind().String())
	case Ptr:
		touch(v.Elem())
	case Struct:
		for i, n := 0, v.NumField(); i < n; i++ {
			// TODO: does this reach unexported fields?
			touch(v.Field(i))
		}
	case Slice, Array:
		for i, n := 0, v.Len(); i < n; i++ {
			touch(v.Index(i))
		}
	case Interface:
		touch(v.Elem())
	case Map:
		iter := v.MapRange()
		for iter.Next() {
			touch(iter.Key())
			touch(iter.Value())
		}
	case String:
	case Bool:
	case Int8, Int16, Int32, Int64, Int:
	case Uint8, Uint16, Uint32, Uint64, Uint, Uintptr:
	case Float32, Float64, Complex64, Complex128:
	}
}
