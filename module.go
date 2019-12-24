package goloader

import (
	"cmd/objfile/goobj"
	"unsafe"
)

//go:linkname firstmoduledata runtime.firstmoduledata
var firstmoduledata moduledata

const PtrSize = 4 << (^uintptr(0) >> 63)
const _funcSize = int(unsafe.Sizeof(_func{}))

type functab struct {
	entry   uintptr
	funcoff uintptr
}

// findfunctab is an array of these structures.
// Each bucket represents 4096 bytes of the text segment.
// Each subbucket represents 256 bytes of the text segment.
// To find a function given a pc, locate the bucket and subbucket for
// that pc. Add together the idx and subbucket value to obtain a
// function index. Then scan the functab array starting at that
// index to find the target function.
// This table uses 20 bytes for every 4096 bytes of code, or ~0.5% overhead.
type findfuncbucket struct {
	idx        uint32
	subbuckets [16]byte
}

// Mapping information for secondary text sections
type textsect struct {
	vaddr    uintptr // prelinked section vaddr
	length   uintptr // section length
	baseaddr uintptr // relocated section address
}

type nameOff int32
type typeOff int32
type textOff int32

// A ptabEntry is generated by the compiler for each exported function
// and global variable in the main package of a plugin. It is used to
// initialize the plugin module's symbol map.
type ptabEntry struct {
	name nameOff
	typ  typeOff
}

type modulehash struct {
	modulename   string
	linktimehash string
	runtimehash  *string
}

type bitvector struct {
	n        int32 // # of bits
	bytedata *uint8
}

type funcInfoData struct {
	_func
	pcdata   []uint32
	funcdata []uintptr
	Var      []goobj.Var
}

type stackmap struct {
	n        int32   // number of bitmaps
	nbit     int32   // number of bits in each bitmap
	bytedata [1]byte // bitmaps, each starting on a byte boundary
}

type Module struct {
	pclntable []byte
	pcfunc    []findfuncbucket
	funcinfo  []funcInfoData
	ftab      []functab // entry need reloc
	filetab   []uint32
	stkmaps   [][]byte
}

const minfunc = 16                 // minimum function size
const pcbucketsize = 256 * minfunc // size of bucket in the pc->func lookup table
const nsub = len(findfuncbucket{}.subbuckets)

//go:linkname step runtime.step
func step(p []byte, pc *uintptr, val *int32, first bool) (newp []byte, ok bool)

//go:linkname findfunc runtime.findfunc
func findfunc(pc uintptr) funcInfo

//go:linkname funcdata runtime.funcdata
func funcdata(f funcInfo, i int32) unsafe.Pointer

//go:linkname funcname runtime.funcname
func funcname(f funcInfo) string

type funcInfo struct {
	*_func
	datap *moduledata
}

func addModule(codeModule *CodeModule, aModule *moduledata) {
	modules[aModule] = true
	for datap := &firstmoduledata; ; {
		if datap.next == nil {
			datap.next = aModule
			break
		}
		datap = datap.next
	}
	codeModule.Module = aModule
}

func removeModule(module interface{}) {
	prevp := &firstmoduledata
	for datap := &firstmoduledata; datap != nil; {
		if datap == module {
			prevp.next = datap.next
			break
		}
		prevp = datap
		datap = datap.next
	}
	delete(modules, module)
}
