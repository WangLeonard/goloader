package goloader

import (
	"bytes"
	"cmd/objfile/goobj"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

func assert(err error) {
	if err != nil {
		panic(err)
	}
}

// copy from $GOROOT/src/cmd/internal/objabi/reloctype.go
const (
	// R_TLS_LE, used on 386, amd64, and ARM, resolves to the offset of the
	// thread-local symbol from the thread local base and is used to implement the
	// "local exec" model for tls access (r.Sym is not set on intel platforms but is
	// set to a TLS symbol -- runtime.tlsg -- in the linker when externally linking).
	R_TLS_LE    = 16
	R_CALL      = 8
	R_CALLARM   = 9
	R_CALLARM64 = 10
	R_CALLIND   = 11
	R_PCREL     = 15
	R_ADDR      = 1
	// R_ADDRARM64 relocates an adrp, add pair to compute the address of the
	// referenced symbol.
	R_ADDRARM64 = 3
	// R_ADDROFF resolves to a 32-bit offset from the beginning of the section
	// holding the data being relocated to the referenced symbol.
	R_ADDROFF = 5
	// R_WEAKADDROFF resolves just like R_ADDROFF but is a weak relocation.
	// A weak relocation does not make the symbol it refers to reachable,
	// and is only honored by the linker if the symbol is in some other way
	// reachable.
	R_WEAKADDROFF = 6
	// R_METHODOFF resolves to a 32-bit offset from the beginning of the section
	// holding the data being relocated to the referenced symbol.
	// It is a variant of R_ADDROFF used when linking from the uncommonType of a
	// *rtype, and may be set to zero by the linker if it determines the method
	// text is unreachable by the linked program.
	R_METHODOFF = 24
)

// copy from $GOROOT/src/cmd/internal/objabi/symkind.go
const (
	// An otherwise invalid zero value for the type
	Sxxx = iota
	// Executable instructions
	STEXT
	// Read only static data
	SRODATA
	// Static data that does not contain any pointers
	SNOPTRDATA
	// Static data
	SDATA
	// Statically data that is initially all 0s
	SBSS
	// Statically data that is initially all 0s and does not contain pointers
	SNOPTRBSS
	// Thread-local data that is initally all 0s
	STLSBSS
	// Debugging data
	SDWARFINFO
	SDWARFRANGE
)

type SymData struct {
	Name   string
	Kind   int
	Offset int
	Reloc  []Reloc
}

type Reloc struct {
	Offset int
	SymOff int
	Size   int
	Type   int
	Add    int
}

// CodeReloc dispatch and load CodeReloc struct via network is OK
type CodeReloc struct {
	Code []byte
	Data []byte
	Mod  Module
	Syms []SymData
}

type CodeModule struct {
	Syms       map[string]uintptr
	CodeByte   []byte
	Module     interface{}
	pcfuncdata []findfuncbucket
	stkmaps    [][]byte
	itabs      []itabReloc
	itabSyms   []itabSym
	typemap    map[typeOff]uintptr
}

type itabSym struct {
	ptr   int
	inter int
	_type int
}

type itabReloc struct {
	locOff  int
	symOff  int
	size    int
	locType int
	add     int
}

type symFile struct {
	sym  *goobj.Sym
	file *os.File
}

type segment struct {
	codeBase   int
	dataBase   int
	codeLen    int
	maxCodeLen int
	offset     int
	symAddrs   []int
	itabMap    map[string]int
	funcType   map[string]*int
	codeByte   []byte
	typeSymPtr map[string]uintptr
	err        bytes.Buffer
}

var (
	modules     = make(map[interface{}]bool)
	modulesLock sync.Mutex
	mov32bit         = [8]byte{0x00, 0x00, 0x80, 0xD2, 0x00, 0x00, 0xA0, 0xF2}
	armcode          = []byte{0x04, 0xF0, 0x1F, 0xE5, 0x00, 0x00, 0x00, 0x00}
	arm64code        = []byte{0x43, 0x00, 0x00, 0x58, 0x60, 0x00, 0x1F, 0xD6, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	x86code          = []byte{0xff, 0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	movcode     byte = 0x8b
	leacode     byte = 0x8d
	cmplcode    byte = 0x83
	jmpcode     byte = 0xe9
)

func ReadObj(f *os.File) (*CodeReloc, error) {
	obj, err := goobj.Parse(f, "main")
	if err != nil {
		return nil, fmt.Errorf("read error: %v", err)
	}

	var syms = make(map[string]symFile)
	for _, sym := range obj.Syms {
		syms[sym.Name] = symFile{
			sym:  sym,
			file: f,
		}
	}

	var symMap = make(map[string]int)
	var gcObjs = make(map[string]uintptr)
	var fileTabOffsetMap = make(map[string]int)

	var reloc CodeReloc

	for _, sym := range obj.Syms {
		if sym.Kind == STEXT && sym.DupOK == false {
			relocSym(&reloc, symFile{sym: sym,
				file: f}, syms, symMap,
				gcObjs, fileTabOffsetMap)
		} else if sym.Kind == SRODATA {
			if strings.HasPrefix(sym.Name, "type.") {
				relocSym(&reloc, symFile{sym: sym,
					file: f}, syms, symMap,
					gcObjs, fileTabOffsetMap)
			}
		}
	}

	return &reloc, nil
}

func ReadObjs(files []string, pkgPath []string) (*CodeReloc, error) {
	var fs []*os.File
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		fs = append(fs, f)
		defer f.Close()
	}

	var allSyms = make(map[string]symFile)

	var symMap = make(map[string]int)
	var gcObjs = make(map[string]uintptr)
	var fileTabOffsetMap = make(map[string]int)

	var reloc CodeReloc

	var goObjs []*goobj.Package
	for i, f := range fs {
		if pkgPath[i] == "" {
			pkgPath[i] = "main"
		}
		obj, err := goobj.Parse(f, pkgPath[i])
		if err != nil {
			return nil, fmt.Errorf("read error: %v", err)
		}

		for _, sym := range obj.Syms {
			allSyms[sym.Name] = symFile{
				sym:  sym,
				file: f,
			}
		}
		goObjs = append(goObjs, obj)
	}

	for i, obj := range goObjs {
		for _, sym := range obj.Syms {
			if sym.Kind == STEXT && sym.DupOK == false {
				relocSym(&reloc, symFile{sym: sym,
					file: fs[i]}, allSyms, symMap,
					gcObjs, fileTabOffsetMap)
			} else if sym.Kind == SRODATA {
				if strings.HasPrefix(sym.Name, "type.") {
					relocSym(&reloc, symFile{sym: sym,
						file: fs[i]}, allSyms, symMap,
						gcObjs, fileTabOffsetMap)
				}
			}
		}
	}

	return &reloc, nil
}

func addSym(symMap map[string]int, symArray *[]SymData, rsym *SymData) int {
	var offset int
	if of, ok := symMap[rsym.Name]; !ok {
		offset = len(*symArray)
		*symArray = append(*symArray, *rsym)
		symMap[rsym.Name] = offset
	} else {
		offset = of
		(*symArray)[offset] = *rsym
	}
	return offset
}

type readAtSeeker struct {
	io.ReadSeeker
}

func (r *readAtSeeker) ReadAt(p []byte, offset int64) (n int, err error) {
	_, err = r.Seek(offset, io.SeekStart)
	if err != nil {
		return
	}
	return r.Read(p)
}

func relocSym(reloc *CodeReloc, curSym symFile,
	allSyms map[string]symFile, symMap map[string]int,
	gcObjs map[string]uintptr, fileTabOffsetMap map[string]int) int {

	if curSymOffset, ok := symMap[curSym.sym.Name]; ok {
		return curSymOffset
	}

	var rsym SymData
	rsym.Name = curSym.sym.Name
	rsym.Kind = int(curSym.sym.Kind)
	curSymOffset := addSym(symMap, &reloc.Syms, &rsym)

	code := make([]byte, curSym.sym.Data.Size)
	curSym.file.Seek(curSym.sym.Data.Offset, io.SeekStart)
	_, err := curSym.file.Read(code)
	assert(err)
	switch int(curSym.sym.Kind) {
	case STEXT:
		rsym.Offset = len(reloc.Code)
		reloc.Code = append(reloc.Code, code...)
		readFuncData(&reloc.Mod, curSym, allSyms, gcObjs,
			fileTabOffsetMap, curSymOffset, rsym.Offset)
	default:
		rsym.Offset = len(reloc.Data)
		reloc.Data = append(reloc.Data, code...)
	}
	addSym(symMap, &reloc.Syms, &rsym)

	for _, re := range curSym.sym.Reloc {
		symOff := -1
		if s, ok := allSyms[re.Sym.Name]; ok {
			symOff = relocSym(reloc, s, allSyms, symMap,
				gcObjs, fileTabOffsetMap)
		} else {
			var exSym SymData
			exSym.Name = re.Sym.Name
			exSym.Offset = -1
			if re.Type == R_TLS_LE {
				exSym.Name = TLSNAME
				exSym.Offset = int(re.Offset)
			}
			if re.Type == R_CALLIND {
				exSym.Offset = 0
				exSym.Name = R_CALLIND_NAME
			}
			if strings.HasPrefix(exSym.Name, "type..importpath.") {
				path := strings.TrimLeft(exSym.Name, "type..importpath.")
				path = strings.Trim(path, ".")
				pathb := []byte(path)
				pathb = append(pathb, 0)
				exSym.Offset = len(reloc.Data)
				reloc.Data = append(reloc.Data, pathb...)
			}
			symOff = addSym(symMap, &reloc.Syms, &exSym)
		}
		rsym.Reloc = append(rsym.Reloc,
			Reloc{Offset: int(re.Offset) + rsym.Offset, SymOff: symOff,
				Type: int(re.Type),
				Size: int(re.Size), Add: int(re.Add)})
	}
	reloc.Syms[curSymOffset].Reloc = rsym.Reloc

	return curSymOffset
}

func strWrite(buf *bytes.Buffer, str ...string) {
	for _, s := range str {
		buf.WriteString(s)
		if s != "\n" {
			buf.WriteString(" ")
		}
	}
}

func relocADRP(mCode []byte, pc int, symAddr int, symName string) {
	pcPage := pc - pc&0xfff
	lowOff := symAddr & 0xfff
	symPage := symAddr - lowOff
	pageOff := symPage - pcPage
	if pageOff > 0x7FFFFFFF || pageOff < -0x80000000 {
		// fmt.Println("adrp overflow!", symName, symAddr, symAddr < (1<<31))
		movlow := binary.LittleEndian.Uint32(mov32bit[:4])
		movhigh := binary.LittleEndian.Uint32(mov32bit[4:])
		adrp := binary.LittleEndian.Uint32(mCode)
		symAddrUint32 := uint32(symAddr)
		movlow = (((adrp & 0x1f) | movlow) | ((symAddrUint32 & 0xffff) << 5))
		movhigh = (((adrp & 0x1f) | movhigh) | ((symAddrUint32 & 0xffff0000) >> 16 << 5))
		// fmt.Println(adrp, movlow, movhigh)
		binary.LittleEndian.PutUint32(mCode, movlow)
		binary.LittleEndian.PutUint32(mCode[4:], movhigh)
		return
	}
	fmt.Println("pageOff<0:", pageOff < 0)
	// 2bit + 19bit + low(12bit) = 33bit
	pageAnd := (uint32((pageOff>>12)&3) << 29) | (uint32((pageOff>>15)&0x7ffff) << 5)

	adrp := binary.LittleEndian.Uint32(mCode)
	adrp = adrp | pageAnd
	binary.LittleEndian.PutUint32(mCode, adrp)

	lowOff = lowOff << 10
	adrpAdd := binary.LittleEndian.Uint32(mCode[4:])
	adrpAdd = adrpAdd | uint32(lowOff)
	binary.LittleEndian.PutUint32(mCode[4:], adrpAdd)
}

func addSymAddrs(code *CodeReloc, symPtr map[string]uintptr, codeModule *CodeModule, seg *segment) {
	for i, sym := range code.Syms {
		if sym.Offset == -1 {
			if ptr, ok := symPtr[sym.Name]; ok {
				seg.symAddrs[i] = int(ptr)
			} else {
				seg.symAddrs[i] = -1
				strWrite(&seg.err, "unresolve external:", sym.Name, "\n")
			}
		} else if sym.Name == TLSNAME {
			RegTLS(symPtr, sym.Offset)
		} else if sym.Kind == STEXT {
			seg.symAddrs[i] = code.Syms[i].Offset + seg.codeBase
			codeModule.Syms[sym.Name] = uintptr(seg.symAddrs[i])
		} else if strings.HasPrefix(sym.Name, "go.itab") {
			if ptr, ok := symPtr[sym.Name]; ok {
				seg.symAddrs[i] = int(ptr)
			} else {
				seg.itabMap[sym.Name] = i
			}
		} else {
			seg.symAddrs[i] = code.Syms[i].Offset + seg.dataBase

			if strings.HasPrefix(sym.Name, "type.func") {
				seg.funcType[sym.Name] = &seg.symAddrs[i]
			}
			if strings.HasPrefix(sym.Name, "type.") {
				if ptr, ok := symPtr[sym.Name]; ok {
					seg.symAddrs[i] = int(ptr)
				} else {
					seg.typeSymPtr[sym.Name] = (uintptr)(seg.symAddrs[i])
				}
			}
		}
	}
}

func addItab(code *CodeReloc, codeModule *CodeModule, seg *segment) {
	for itabName, itabIndex := range seg.itabMap {
		curSym := code.Syms[itabIndex]
		inter := seg.symAddrs[curSym.Reloc[0].SymOff]
		_type := seg.symAddrs[curSym.Reloc[1].SymOff]
		if inter == -1 || _type == -1 {
			continue
		}
		seg.itabMap[itabName] = len(codeModule.itabSyms)
		codeModule.itabSyms = append(codeModule.itabSyms, itabSym{inter: inter, _type: _type})

		addIFaceSubFuncType(seg.funcType, codeModule.typemap,
			(*interfacetype)(unsafe.Pointer(uintptr(inter))), seg.codeBase)
	}
}

func relocateItab(code *CodeReloc, codeModule *CodeModule, seg *segment) {
	for i := range codeModule.itabSyms {
		it := &codeModule.itabSyms[i]
		it.ptr = getitab(it.inter, it._type, false)
	}

	for _, it := range codeModule.itabs {
		symAddr := codeModule.itabSyms[it.symOff].ptr
		if symAddr == 0 {
			continue
		}
		switch it.locType {
		case R_PCREL:
			pc := seg.codeBase + it.locOff + it.size
			offset := symAddr - pc + it.add
			if offset > 0x7FFFFFFF || offset < -0x80000000 {
				offset = (seg.codeBase + seg.offset) - pc + it.add
				binary.LittleEndian.PutUint32(seg.codeByte[it.locOff:], uint32(offset))
				seg.codeByte[it.locOff-2:][0] = movcode
				*(*uintptr)(unsafe.Pointer(&(seg.codeByte[seg.offset:][0]))) = uintptr(symAddr)
				seg.offset += PtrSize
				continue
			}
			binary.LittleEndian.PutUint32(seg.codeByte[it.locOff:], uint32(offset))
		case R_ADDRARM64:
			relocADRP(seg.codeByte[it.locOff:], seg.codeBase+it.locOff, symAddr, "unknown")
		}
	}
}

func relocate(code *CodeReloc, symPtr map[string]uintptr, codeModule *CodeModule, seg *segment) {
	for _, curSym := range code.Syms {
		for _, loc := range curSym.Reloc {
			sym := code.Syms[loc.SymOff]
			if seg.symAddrs[loc.SymOff] == -1 {
				continue
			}
			if seg.symAddrs[loc.SymOff] == 0 && strings.HasPrefix(sym.Name, "go.itab") {
				codeModule.itabs = append(codeModule.itabs,
					itabReloc{locOff: loc.Offset, symOff: seg.itabMap[sym.Name],
						size: loc.Size, locType: loc.Type, add: loc.Add})
				continue
			}

			var offset int
			switch loc.Type {
			case R_TLS_LE:
				binary.LittleEndian.PutUint32(code.Code[loc.Offset:], uint32(symPtr[TLSNAME]))
				continue
			case R_CALL, R_PCREL:
				var relocByte = code.Data
				var addrBase = seg.dataBase
				if curSym.Kind == STEXT {
					addrBase = seg.codeBase
					relocByte = code.Code
				}
				offset = seg.symAddrs[loc.SymOff] - (addrBase + loc.Offset + loc.Size) + loc.Add
				if offset > 0x7fffffff || offset < -0x8000000 {
					if seg.offset+8 > seg.maxCodeLen {
						strWrite(&seg.err, "len overflow", "sym:", sym.Name, "\n")
						continue
					}
					rb := relocByte[loc.Offset-2:]
					if loc.Type == R_CALL {
						offset = (seg.codeBase + seg.offset) - (addrBase + loc.Offset + loc.Size)
						copy(seg.codeByte[seg.offset:], x86code)
						binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
						if uint64(seg.symAddrs[loc.SymOff]+loc.Add) > 0xFFFFFFFF {
							binary.LittleEndian.PutUint64(seg.codeByte[seg.offset+6:], uint64(seg.symAddrs[loc.SymOff]+loc.Add))
						} else {
							binary.LittleEndian.PutUint32(seg.codeByte[seg.offset+6:], uint32(seg.symAddrs[loc.SymOff]+loc.Add))
						}
						seg.offset += len(x86code)
					} else if rb[0] == leacode || rb[0] == movcode || rb[0] == cmplcode || rb[1] == jmpcode {
						offset = (seg.codeBase + seg.offset) - (addrBase + loc.Offset + loc.Size)
						binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
						if rb[0] == leacode {
							rb[0] = movcode
						}
						if uint64(seg.symAddrs[loc.SymOff]+loc.Add) > 0xFFFFFFFF {
							binary.LittleEndian.PutUint64(seg.codeByte[seg.offset:], uint64(seg.symAddrs[loc.SymOff]+loc.Add))
							seg.offset += 12
						} else {
							binary.LittleEndian.PutUint32(seg.codeByte[seg.offset:], uint32(seg.symAddrs[loc.SymOff]+loc.Add))
							seg.offset += 8
						}
					} else {
						strWrite(&seg.err, "offset overflow sym:", sym.Name, "\n")
						binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
					}
					continue
				}
				binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
			case R_CALLARM, R_CALLARM64:
				var add = loc.Add
				var pcOff = 0
				if loc.Type == R_CALLARM {
					add = loc.Add & 0xffffff
					if add > 256 {
						add = 0
					} else {
						add += 2
					}
					pcOff = 8
				}
				offset = (seg.symAddrs[loc.SymOff] - (seg.codeBase + loc.Offset + pcOff) + add) / 4
				if offset > 0x7FFFFF || offset < -0x800000 {
					if seg.offset+4 > seg.maxCodeLen {
						strWrite(&seg.err, "len overflow", "sym:", sym.Name, "\n")
						continue
					}
					align := seg.offset % 4
					if align != 0 {
						seg.offset += (4 - align)
					}
					offset = (seg.offset - (loc.Offset + pcOff)) / 4
					var v = uint32(offset)
					b := code.Code[loc.Offset:]
					b[0] = byte(v)
					b[1] = byte(v >> 8)
					b[2] = byte(v >> 16)
					var jmpLocOff = 0
					var jmpLen = 0
					if loc.Type == R_CALLARM64 {
						copy(seg.codeByte[seg.offset:], arm64code)
						jmpLen = len(arm64code)
						jmpLocOff = 8
					} else {
						copy(seg.codeByte[seg.offset:], armcode)
						jmpLen = len(armcode)
						jmpLocOff = 4
					}
					*(*uintptr)(unsafe.Pointer(&(seg.codeByte[seg.offset+jmpLocOff:][0]))) = uintptr(seg.symAddrs[loc.SymOff] + add*4)
					seg.offset += jmpLen
					continue
				}
				var v = uint32(offset)
				b := code.Code[loc.Offset:]
				b[0] = byte(v)
				b[1] = byte(v >> 8)
				b[2] = byte(v >> 16)
			case R_ADDRARM64:
				if curSym.Kind != STEXT {
					strWrite(&seg.err, "not in code?\n")
				}
				relocADRP(code.Code[loc.Offset:], seg.codeBase+loc.Offset, seg.symAddrs[loc.SymOff], sym.Name)
			case R_ADDR:
				var relocByte = code.Data
				if curSym.Kind == STEXT {
					relocByte = code.Code
				}
				offset = seg.symAddrs[loc.SymOff] + loc.Add
				*(*uintptr)(unsafe.Pointer(&(relocByte[loc.Offset:][0]))) = uintptr(offset)
			case R_CALLIND:

			case R_ADDROFF, R_WEAKADDROFF, R_METHODOFF:
				var relocByte = code.Data
				var addrBase = seg.codeBase
				if curSym.Kind == STEXT {
					strWrite(&seg.err, "impossible!", sym.Name, "locate on code segment", "\n")
				}
				offset = seg.symAddrs[loc.SymOff] - addrBase + loc.Add
				binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
			default:
				strWrite(&seg.err, "unknown reloc type:", strconv.Itoa(loc.Type), sym.Name, "\n")
			}

		}
	}
}

func buildModule(code *CodeReloc, symPtr map[string]uintptr, codeModule *CodeModule, seg *segment) {
	var module moduledata
	module.ftab = make([]functab, len(code.Mod.ftab))
	copy(module.ftab, code.Mod.ftab)
	pclnOff := len(code.Mod.pclntable)
	module.pclntable = make([]byte, len(code.Mod.pclntable)+
		(_funcSize+128)*len(code.Mod.ftab))
	copy(module.pclntable, code.Mod.pclntable)
	module.findfunctab = (uintptr)(unsafe.Pointer(&code.Mod.pcfunc[0]))
	module.minpc = (uintptr)(unsafe.Pointer(&seg.codeByte[0]))
	module.maxpc = (uintptr)(unsafe.Pointer(&seg.codeByte[len(code.Code)-1])) + 2
	module.filetab = code.Mod.filetab
	module.typemap = codeModule.typemap
	module.types = uintptr(seg.codeBase)
	module.etypes = uintptr(seg.codeBase + seg.codeLen)
	module.text = uintptr(seg.codeBase)
	module.etext = uintptr(seg.codeBase + len(code.Code))
	codeModule.pcfuncdata = code.Mod.pcfunc // hold reference
	codeModule.stkmaps = code.Mod.stkmaps
	for i := range module.ftab {
		pclnOff = addFuncTab(&module, i, pclnOff, code, seg, symPtr)
	}
	module.pclntable = module.pclntable[:pclnOff]
	module.ftab = append(module.ftab, functab{})
	for i := len(module.ftab) - 1; i > 0; i-- {
		module.ftab[i] = module.ftab[i-1]
	}
	module.ftab = append(module.ftab, functab{})
	module.ftab[0].entry = module.minpc
	module.ftab[len(module.ftab)-1].entry = module.maxpc

	modulesLock.Lock()
	addModule(codeModule, &module)
	modulesLock.Unlock()

	copy(seg.codeByte, code.Code)
	copy(seg.codeByte[len(code.Code):], code.Data)
	codeModule.CodeByte = seg.codeByte
}

func Load(code *CodeReloc, symPtr map[string]uintptr) (*CodeModule, error) {
	var seg segment
	seg.codeLen = len(code.Code) + len(code.Data)
	seg.maxCodeLen = seg.codeLen * 2
	codeByte, err := Mmap(seg.maxCodeLen)
	if err != nil {
		return nil, err
	}
	seg.codeByte = codeByte

	var codeModule = CodeModule{
		Syms:    make(map[string]uintptr),
		typemap: make(map[typeOff]uintptr),
	}

	seg.codeBase = int((*sliceHeader)(unsafe.Pointer(&codeByte)).Data)
	seg.dataBase = seg.codeBase + len(code.Code)
	seg.symAddrs = make([]int, len(code.Syms))
	seg.funcType = make(map[string]*int)
	seg.itabMap = make(map[string]int)
	seg.typeSymPtr = make(map[string]uintptr)
	seg.offset = seg.codeLen

	addSymAddrs(code, symPtr, &codeModule, &seg)
	addItab(code, &codeModule, &seg)
	relocate(code, symPtr, &codeModule, &seg)
	buildModule(code, symPtr, &codeModule, &seg)
	relocateItab(code, &codeModule, &seg)

	if seg.err.Len() > 0 {
		return &codeModule, errors.New(seg.err.String())
	}
	return &codeModule, nil
}

func copy2Slice(dst []byte, src unsafe.Pointer, size int) {
	var s = sliceHeader{
		Data: (uintptr)(src),
		Len:  size,
		Cap:  size,
	}
	copy(dst, *(*[]byte)(unsafe.Pointer(&s)))
}

func (cm *CodeModule) Unload() {
	runtime.GC()
	modulesLock.Lock()
	removeModule(cm.Module)
	modulesLock.Unlock()
	Munmap(cm.CodeByte)
}
