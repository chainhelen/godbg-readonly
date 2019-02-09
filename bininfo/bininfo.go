package bininfo

import (
	"bytes"
	"compress/zlib"
	"debug/dwarf"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"github.com/go-delve/delve/pkg/dwarf/line"
	"github.com/go-delve/delve/pkg/dwarf/op"
	"github.com/go-delve/delve/pkg/goversion"
	"godbg/log"
	"io"
	"io/ioutil"
	alog "log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const dwarfGoLanguage = 22 // DW_LANG_Go (from DWARF v5, section 7.12, page 231)
var logger *alog.Logger

func init() {
	logger = log.Logger
}

type Function struct {
	Name       string
	Entry, End uint64
	offset     dwarf.Offset
	cu         *compileUnit
}

const (
	_AT_NULL_AMD64  = 0
	_AT_ENTRY_AMD64 = 9
)

// copy from dlv partly
type BinaryInfo struct {
	// Path on disk of the binary being executed.
	Path string
	// Architecture of this binary.
	// Arch Arch

	// GOOS operating system this binary is executing on.
	GOOS string

	// Functions is a list of all DW_TAG_subprogram entries in debug_info, sorted by entry point
	Functions []Function
	// Sources is a list of all source files found in debug_line.
	Sources []string
	// LookupFunc maps function names to a description of the function.
	LookupFunc map[string]*Function

	lastModified time.Time // Time the executable of this process was last modified

	// Maps package names to package paths, needed to lookup types inside DWARF info
	packageMap map[string]string

	dwarf        *dwarf.Data
	dwarfReader  *dwarf.Reader
	compileUnits []*compileUnit
}

type compileUnit struct {
	name   string // univocal name for non-go compile units
	lowPC  uint64
	ranges [][2]uint64

	entry    *dwarf.Entry        // debug_info entry describing this compile unit
	isgo     bool                // true if this is the go compile unit
	lineInfo *line.DebugLineInfo // debug_line segment associated with this compile unit
	//concreteInlinedFns []inlinedFn         // list of concrete inlined functions within this compile unit
	optimized bool   // this compile unit is optimized
	producer  string // producer attribute

	startOffset, endOffset dwarf.Offset // interval of offsets contained in this compile unit
}

// packageVar represents a package-level variable (or a C global variable).
// If a global variable does not have an address (for example it's stored in
// a register, or non-contiguously) addr will be 0.
type packageVar struct {
	name   string
	offset dwarf.Offset
	addr   uint64
}

type partialUnitConstant struct {
	name  string
	typ   dwarf.Offset
	value int64
}

// type partialUnit struct {
// 	entry     *dwarf.Entry
// 	types     map[string]dwarf.Offset
// 	variables []packageVar
// 	constants []partialUnitConstant
// 	functions []Function
// }

func entryPointFromAuxvAMD64(auxv []byte) uint64 {
	rd := bytes.NewBuffer(auxv)

	for {
		var tag, val uint64
		err := binary.Read(rd, binary.LittleEndian, &tag)
		if err != nil {
			return 0
		}
		err = binary.Read(rd, binary.LittleEndian, &val)
		if err != nil {
			return 0
		}

		switch tag {
		case _AT_NULL_AMD64:
			return 0
		case _AT_ENTRY_AMD64:
			return val
		}
	}
}

func decompressMaybe(b []byte) ([]byte, error) {
	if len(b) < 12 || string(b[:4]) != "ZLIB" {
		return b, nil
	}

	dlen := binary.BigEndian.Uint64(b[4:12])
	dbuf := make([]byte, dlen)
	r, err := zlib.NewReader(bytes.NewBuffer(b[12:]))
	if err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, dbuf); err != nil {
		return nil, err
	}
	if err := r.Close(); err != nil {
		return nil, err
	}
	return dbuf, nil
}

func GetDebugSectionElf(f *elf.File, name string) ([]byte, error) {
	sec := f.Section(".debug_" + name)
	if sec != nil {
		return sec.Data()
	}
	sec = f.Section(".zdebug_" + name)
	if sec == nil {
		return nil, fmt.Errorf("could not find .debug_%s section", name)
	}
	b, err := sec.Data()
	if err != nil {
		return nil, err
	}
	return decompressMaybe(b)
}

func LoadBinInfo(debugFile string, process *os.Process) (bi *BinaryInfo) {
	bi = &BinaryInfo{}
	var (
		auxvbuf        []byte
		err            error
		entryPoint     uint64
		fi             os.FileInfo
		debugLineBytes []byte
		exe            *os.File
		elfFile        *elf.File
		dwarfD         *dwarf.Data
		dwarfReader    *dwarf.Reader
	)
	logger.Printf("\t[LoadBinInfo] read /proc/%d/auxv\n", process.Pid)
	auxvbuf, err = ioutil.ReadFile(fmt.Sprintf("/proc/%d/auxv", process.Pid))
	if err != nil {
		logger.Printf("\t[LoadBinInfo] err: %s\n", err.Error())
		panic(err)
	}
	entryPoint = entryPointFromAuxvAMD64(auxvbuf)
	logger.Printf("\t[LoadBinInfo] read entryPoint %d\n", entryPoint)

	fi, err = os.Stat(debugFile)
	if err == nil {
		lastModified := fi.ModTime()
		logger.Printf("\t[LoadBinInfo] read lastModified %v\n", lastModified)
	}
	exe, err = os.OpenFile(debugFile, 0, os.ModePerm)
	if err != nil {
		logger.Printf("\t[LoadBinInfo] OpenFile:%s failed, err: %s\n", debugFile, err.Error())
		panic(err)
	}
	logger.Printf("\t[LoadBinInfo] OpenFile:%s sucessfully\n", debugFile)

	elfFile, err = elf.NewFile(exe)
	if err != nil {
		panic(err)
	}
	// godwarf, err = elfFile.DWARF()
	// if err != nil {
	// 	panic(err)
	// }
	debugLineBytes, err = GetDebugSectionElf(elfFile, "line")
	if err != nil {
		panic(err)
	}

	dwarfD, err = elfFile.DWARF()
	if err != nil {
		panic(err)
	}

	dwarfReader = dwarfD.Reader()
	bi.loadDebugInfoMaps(dwarfD, dwarfReader, debugLineBytes)

	logger.Printf("\t[LoadBinInfo] debugLineBytes sucessfully \n")
	return bi
}

func (bi *BinaryInfo) loadDebugInfoMaps(dwarfD *dwarf.Data, dwarfReader *dwarf.Reader, debugLineBytes []byte) {
	compileUnits := []*compileUnit{}
	packageVars := []packageVar{}
	functions := []Function{}

	var cu *compileUnit
	for entry, err := dwarfReader.Next(); entry != nil; entry, err = dwarfReader.Next() {
		if err != nil {
			panic(err)
		}
		switch entry.Tag {
		case dwarf.TagCompileUnit:
			if cu != nil {
				cu.endOffset = entry.Offset
			}
			cu = &compileUnit{}
			cu.entry = entry
			cu.startOffset = entry.Offset
			if lang, _ := entry.Val(dwarf.AttrLanguage).(int64); lang == dwarfGoLanguage {
				cu.isgo = true
			}
			cu.name, _ = entry.Val(dwarf.AttrName).(string)

			compdir, _ := entry.Val(dwarf.AttrCompDir).(string)
			if compdir != "" {
				cu.name = filepath.Join(compdir, cu.name)
			}
			cu.ranges, _ = dwarfD.Ranges(entry)
			for i := range cu.ranges {
				cu.ranges[i][0] += 0
				cu.ranges[i][1] += 0
			}
			if len(cu.ranges) >= 1 {
				cu.lowPC = cu.ranges[0][0]
			}
			lineInfoOffset, _ := entry.Val(dwarf.AttrStmtList).(int64)
			if lineInfoOffset >= 0 && lineInfoOffset < int64(len(debugLineBytes)) {
				var logfn func(string, ...interface{})
				cu.lineInfo = line.Parse(compdir, bytes.NewBuffer(debugLineBytes[lineInfoOffset:]), logfn)
			}
			cu.producer, _ = entry.Val(dwarf.AttrProducer).(string)
			if cu.isgo && cu.producer != "" {
				semicolon := strings.Index(cu.producer, ";")
				if semicolon < 0 {
					cu.optimized = goversion.ProducerAfterOrEqual(cu.producer, 1, 10)
				} else {
					cu.optimized = !strings.Contains(cu.producer[semicolon:], "-N") || !strings.Contains(cu.producer[semicolon:], "-l")
					cu.producer = cu.producer[:semicolon]
				}
			}
			compileUnits = append(compileUnits, cu)

			// logger.Printf("\t[loadDebugInfoMaps] dwarf.TagCompileUnit cu:%#v\n", cu)
		case dwarf.TagPartialUnit:
			logger.Printf("\t[loadDebugInfoMaps] not support dwarf.TagPartialUnit\n")
			panic("not support dwarf.TagPartialUnit")
		case dwarf.TagImportedUnit:
			logger.Printf("\t[loadDebugInfoMaps] not support dwarf.TagImportedUnit\n")
			panic("not support dwarf.TagImportedUnit")
		case dwarf.TagVariable:
			if n, ok := entry.Val(dwarf.AttrName).(string); ok {
				var addr uint64
				if loc, ok := entry.Val(dwarf.AttrLocation).([]byte); ok {
					// if len(loc) == bi.Arch.PtrSize()+1 && op.Opcode(loc[0]) == op.DW_OP_addr {
					if len(loc) == 8+1 && op.Opcode(loc[0]) == op.DW_OP_addr {
						addr = binary.LittleEndian.Uint64(loc[1:])
						logger.Printf("\t[loadDebugInfoMaps] dwarf.TagVariable n:%s loc:%#v \n", n, loc)
					}
				}
				// packageVars = append(packageVars, packageVar{n, entry.Offset, addr + bi.staticBase})
				packageVars = append(packageVars, packageVar{n, entry.Offset, addr + 0})
			}
		case dwarf.TagSubprogram:
			var lowpc, highpc uint64
			var ok bool
			var name string
			if ranges, _ := dwarfD.Ranges(entry); len(ranges) == 1 {
				// lowpc = ranges[0][0] + bi.staticBase
				// highpc = ranges[0][1] + bi.staticBase
				lowpc = ranges[0][0] + 0
				highpc = ranges[0][1] + 0
			}
			if name, ok = entry.Val(dwarf.AttrName).(string); !ok {
				logger.Printf("\t[loadDebugInfoMaps] not support dwarf.TagSubprogram without dwarf.AttrName\n")
				panic("not support dwarf.TagSubprogram without dwarf.AttrName")
			}

			var fn Function
			if ok {
				fn = Function{
					Name:  name,
					Entry: lowpc, End: highpc,
					offset: entry.Offset,
					cu:     cu,
				}
				functions = append(functions, fn)
			}
		}
	}
	bi.compileUnits = compileUnits
	for i := 0; i < len(compileUnits); i++ {
		logger.Printf("\t[loadDebugInfoMaps] readAfterFor compileUnits[%d]:%#v \n", i, compileUnits[i])
		cu = compileUnits[i]
		dLI := cu.lineInfo
		lookup := dLI.Lookup
		for i, v := range lookup {
			logger.Printf("\t[loadDebugInfoMaps] readAfterFor compileUnits[%d]: fileNames%#v \n", i, v)
		}
	}

	bi.Sources = []string{}
	for _, cu := range bi.compileUnits {
		if cu.lineInfo != nil {
			for _, fileEntry := range cu.lineInfo.FileNames {
				bi.Sources = append(bi.Sources, fileEntry.Path)
			}
		}
	}
	sort.Strings(bi.Sources)
	bi.Sources = uniq(bi.Sources)

	for i := 0; i < len(bi.Sources); i++ {
		logger.Printf("\t[loadDebugInfoMaps] readAfterFor Sources[%d]:%#v \n", i, bi.Sources[i])
	}

	for i := 0; i < len(packageVars); i++ {
		logger.Printf("\t[loadDebugInfoMaps] readAfterFor packageVars[%d]:%#v \n", i, packageVars[i])
	}
	for i := 0; i < len(functions); i++ {
		logger.Printf("\t[loadDebugInfoMaps] readAfterFor functions[%d]:%#v \n", i, functions[i])
	}
}

func uniq(s []string) []string {
	if len(s) <= 0 {
		return s
	}
	src, dst := 1, 1
	for src < len(s) {
		if s[src] != s[dst-1] {
			s[dst] = s[src]
			dst++
		}
		src++
	}
	return s[:dst]
}

func partialPathMatch(expr, path string) bool {
	if len(expr) < len(path)-1 {
		return strings.HasSuffix(path, expr) && (path[len(path)-len(expr)-1] == '/')
	}
	return expr == path
}

func (bi *BinaryInfo) FindLocationFromFileLoc(filename string, lineno string) (uint64, error) {
	var (
		num            int
		err            error
		absfilename    string
		candidateFiles []string
	)
	for _, file := range bi.Sources {
		if partialPathMatch(filename, file) {
			candidateFiles = append(candidateFiles, file)
			if len(candidateFiles) >= 3 {
				break
			}
		}
	}

	if len(candidateFiles) != 1 {
		return 0, fmt.Errorf("Counld not find right filename %s", strings.Join(candidateFiles, ";"))
	}
	absfilename = candidateFiles[0]

	num, err = strconv.Atoi(lineno)
	if err != nil {
		return 0, fmt.Errorf("Wrong lineno %s", lineno)
	}
	for _, cu := range bi.compileUnits {
		if cu.lineInfo.Lookup[absfilename] != nil {
			pc := cu.lineInfo.LineToPC(absfilename, num)
			if pc != 0 {
				return pc, nil
			}
		}
	}
	return 0, fmt.Errorf("could not find %s:%s", filename, lineno)
}
