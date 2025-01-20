package symbol

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"unsafe"

	"github.com/goplus/llcppg/_xtool/llcppsymg/config"
	"github.com/goplus/llcppg/_xtool/llcppsymg/config/cfgparse"
	"github.com/goplus/llcppg/_xtool/llcppsymg/dbg"
	"github.com/goplus/llcppg/_xtool/llcppsymg/parse"
	"github.com/goplus/llcppg/types"
	"github.com/goplus/llgo/c"
	"github.com/goplus/llgo/c/cjson"
	"github.com/goplus/llgo/xtool/nm"
)

// ParseDylibSymbols parses symbols from dynamic libraries specified in the lib string.
// It handles multiple libraries (e.g., -L/opt/homebrew/lib -llua -lm) and returns
// symbols if at least one library is successfully parsed. Errors from inaccessible
// libraries (like standard libs) are logged as warnings.
//
// Returns symbols and nil error if any symbols are found, or nil and error if none found.
func ParseDylibSymbols(lib string) ([]*nm.Symbol, error) {
	if dbg.GetDebugSymbol() {
		fmt.Println("ParseDylibSymbols:from", lib)
	}
	sysPaths := getSysLibPaths()
	lbs := cfgparse.ParseLibs(lib)
	if dbg.GetDebugSymbol() {
		fmt.Println("ParseDylibSymbols:LibConfig Parse To")
		fmt.Println("libs.Names: ", lbs.Names)
		fmt.Println("libs.Paths: ", lbs.Paths)
	}
	dylibPaths, notFounds, err := lbs.GenDylibPaths(sysPaths)
	if err != nil {
		return nil, fmt.Errorf("failed to generate some dylib paths: %v", err)
	}

	if dbg.GetDebugSymbol() {
		fmt.Println("ParseDylibSymbols:dylibPaths", dylibPaths)
		if len(notFounds) > 0 {
			fmt.Println("ParseDylibSymbols:not found libname", notFounds)
		} else {
			fmt.Println("ParseDylibSymbols:every library is found")
		}
	}

	var symbols []*nm.Symbol
	var parseErrors []string

	for _, dylibPath := range dylibPaths {
		if _, err := os.Stat(dylibPath); err != nil {
			if dbg.GetDebugSymbol() {
				fmt.Printf("ParseDylibSymbols:Failed to access dylib %s: %v\n", dylibPath, err)
			}
			continue
		}

		args := []string{}
		if runtime.GOOS == "linux" {
			args = append(args, "-D")
		}

		files, err := nm.New("").List(dylibPath, args...)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("ParseDylibSymbols:Failed to list symbols in dylib %s: %v", dylibPath, err))
			continue
		}

		for _, file := range files {
			symbols = append(symbols, file.Symbols...)
		}
	}

	if len(symbols) > 0 {
		if dbg.GetDebugSymbol() {
			if len(parseErrors) > 0 {
				fmt.Printf("ParseDylibSymbols:Some libraries could not be parsed: %v\n", parseErrors)
			}
			fmt.Println("ParseDylibSymbols:", len(symbols), "symbols")
		}
		return symbols, nil
	}

	return nil, fmt.Errorf("no symbols found in any dylib. Errors: %v", parseErrors)
}

func getSysLibPaths() []string {
	var paths []string
	if runtime.GOOS == "linux" {
		if dbg.GetDebugSymbol() {
			fmt.Println("getSysLibPaths:find sys lib path from linux")
		}
		//resolution from https://github.com/goplus/llcppg/commit/02307485db9269481297a4dc5e8449fffaa4f562
		cmd := exec.Command("ld", "--verbose")
		output, err := cmd.Output()
		if err != nil {
			panic(err)
		}
		matches := regexp.MustCompile(`SEARCH_DIR\("=([^"]+)"\)`).FindAllStringSubmatch(string(output), -1)
		for _, match := range matches {
			paths = append(paths, match[1])
		}
		return paths
	}
	return paths
}

func getPath(file string) []string {
	if dbg.GetDebugSymbol() {
		fmt.Println("getPath:from", file)
	}
	var paths []string
	content, err := os.ReadFile(file)
	if err != nil {
		return paths
	}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			if file, err := os.Stat(line); err == nil && file.IsDir() {
				paths = append(paths, line)
			}
		}
	}
	return paths
}

// finds the intersection of symbols from the dynamic library's symbol table and the symbols parsed from header files.
// It returns a list of symbols that can be externally linked.
func GetCommonSymbols(dylibSymbols []*nm.Symbol, headerSymbols map[string]*parse.SymbolInfo) []*types.SymbolInfo {
	var commonSymbols []*types.SymbolInfo
	for _, dylibSym := range dylibSymbols {
		symName := dylibSym.Name
		if runtime.GOOS == "darwin" {
			symName = strings.TrimPrefix(symName, "_")
		}
		if symInfo, ok := headerSymbols[symName]; ok {
			symbolInfo := &types.SymbolInfo{
				Mangle: symName,
				CPP:    symInfo.ProtoName,
				Go:     symInfo.GoName,
			}
			commonSymbols = append(commonSymbols, symbolInfo)
		}
	}
	return commonSymbols
}

func ReadExistingSymbolTable(fileName string) (map[string]types.SymbolInfo, bool) {
	if _, err := os.Stat(fileName); err != nil {
		return nil, false
	}

	data, err := os.ReadFile(fileName)
	if err != nil {
		return nil, false
	}

	parsedJSON := cjson.ParseBytes(data)
	if parsedJSON == nil {
		return nil, false
	}

	existingSymbols := make(map[string]types.SymbolInfo)
	arraySize := parsedJSON.GetArraySize()

	for i := 0; i < int(arraySize); i++ {
		item := parsedJSON.GetArrayItem(c.Int(i))
		symbol := types.SymbolInfo{
			Mangle: config.GetStringItem(item, "mangle", ""),
			CPP:    config.GetStringItem(item, "c++", ""),
			Go:     config.GetStringItem(item, "go", ""),
		}
		existingSymbols[symbol.Mangle] = symbol
	}

	return existingSymbols, true
}

func GenSymbolTableData(commonSymbols []*types.SymbolInfo, existingSymbols map[string]types.SymbolInfo) ([]byte, error) {
	if len(existingSymbols) > 0 {
		if dbg.GetDebugSymbol() {
			fmt.Println("GenSymbolTableData:generate symbol table with exist symbol table")
		}
		for i := range commonSymbols {
			if existingSymbol, exists := existingSymbols[commonSymbols[i].Mangle]; exists && commonSymbols[i].Go != existingSymbol.Go {
				if dbg.GetDebugSymbol() {
					fmt.Println("symbol", commonSymbols[i].Mangle, "already exist, use exist symbol", existingSymbol.Go)
				}
				commonSymbols[i].Go = existingSymbol.Go
			} else {
				if dbg.GetDebugSymbol() {
					fmt.Println("new symbol", commonSymbols[i].Mangle, "-", commonSymbols[i].CPP, "-", commonSymbols[i].Go)
				}
			}
		}
	} else {
		if dbg.GetDebugSymbol() {
			fmt.Println("GenSymbolTableData:generate symbol table without symbol table")
			for _, symbol := range commonSymbols {
				fmt.Println("new symbol", symbol.Mangle, "-", symbol.CPP, "-", symbol.Go)
			}
		}
	}

	root := cjson.Array()
	defer root.Delete()

	for _, symbol := range commonSymbols {
		item := cjson.Object()
		item.SetItem(c.Str("mangle"), cjson.String(c.AllocaCStr(symbol.Mangle)))
		item.SetItem(c.Str("c++"), cjson.String(c.AllocaCStr(symbol.CPP)))
		item.SetItem(c.Str("go"), cjson.String(c.AllocaCStr(symbol.Go)))
		root.AddItem(item)
	}

	cStr := root.Print()
	if cStr == nil {
		return nil, errors.New("symbol table is empty")
	}
	defer c.Free(unsafe.Pointer(cStr))
	result := []byte(c.GoString(cStr))
	return result, nil
}

func GenerateAndUpdateSymbolTable(symbols []*nm.Symbol, headerInfos map[string]*parse.SymbolInfo, symbFile string) ([]byte, error) {
	commonSymbols := GetCommonSymbols(symbols, headerInfos)
	if dbg.GetDebugSymbol() {
		fmt.Println("GenerateAndUpdateSymbolTable:", len(commonSymbols), "common symbols")
	}

	existSymbols, exist := ReadExistingSymbolTable(symbFile)
	if exist && dbg.GetDebugSymbol() {
		fmt.Println("GenerateAndUpdateSymbolTable:current path have exist symbol table", symbFile)
	}

	symbolData, err := GenSymbolTableData(commonSymbols, existSymbols)
	if err != nil {
		return nil, err
	}

	return symbolData, nil
}

// For mutiple os test,the nm output's symbol name is different.
func AddSymbolPrefixUnder(name string, isCpp bool) string {
	prefix := ""
	if runtime.GOOS == "darwin" {
		prefix = prefix + "_"
	}
	if isCpp {
		prefix = prefix + "_"
	}
	return prefix + name
}
