// +build go1.8 go1.9 go1.10 go1.11
// +build !go1.12,!go1.13

package goloader

import (
	"unsafe"
)

func addFuncTab(module *moduledata, i, pclnOff int, code *CodeReloc, seg *segment, symPtr map[string]uintptr) int {
	module.ftab[i].entry = uintptr(seg.symAddrs[int(code.Mod.ftab[i].entry)])

	ptr2 := (uintptr)(unsafe.Pointer(&module.pclntable[pclnOff]))
	if PtrSize == 8 && ptr2&4 != 0 {
		pclnOff += 4
	}
	module.ftab[i].funcoff = uintptr(pclnOff)
	fi := code.Mod.funcinfo[i]
	fi.entry = module.ftab[i].entry
	copy2Slice(module.pclntable[pclnOff:], unsafe.Pointer(&fi._func), _funcSize)
	pclnOff += _funcSize

	if len(fi.pcdata) > 0 {
		size := int(4 * fi.npcdata)
		copy2Slice(module.pclntable[pclnOff:], unsafe.Pointer(&fi.pcdata[0]), size)
		pclnOff += size
	}

	var funcdata = make([]uintptr, len(fi.funcdata))
	copy(funcdata, fi.funcdata)
	for i, v := range funcdata {
		if v != 0 {
			funcdata[i] = (uintptr)(unsafe.Pointer(&(code.Mod.stkmaps[v][0])))
		} else {
			funcdata[i] = (uintptr)(0)
		}
	}
	ptr := (uintptr)(unsafe.Pointer(&module.pclntable[pclnOff-1])) + 1
	if PtrSize == 8 && ptr&4 != 0 {
		t := [4]byte{}
		copy(module.pclntable[pclnOff:], t[:])
		pclnOff += len(t)
	}
	funcDataSize := int(PtrSize * fi.nfuncdata)
	copy2Slice(module.pclntable[pclnOff:], unsafe.Pointer(&funcdata[0]), funcDataSize)
	pclnOff += funcDataSize

	return pclnOff
}
