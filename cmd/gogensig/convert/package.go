package convert

import (
	"bytes"
	"fmt"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"github.com/goplus/gogen"
	"github.com/goplus/llcppg/_xtool/llcppsymg/config/cfgparse"
	"github.com/goplus/llcppg/ast"
	cfg "github.com/goplus/llcppg/cmd/gogensig/config"
	"github.com/goplus/llcppg/cmd/gogensig/convert/deps"
	"github.com/goplus/llcppg/cmd/gogensig/convert/names"
	cppgtypes "github.com/goplus/llcppg/types"
)

const (
	DbgFlagAll = 1
)

var (
	debug bool
)

func SetDebug(flags int) {
	if flags != 0 {
		debug = true
	}
}

type Package struct {
	name       string         // package name
	p          *gogen.Package // package writer
	conf       *PackageConfig // package config
	cvt        *TypeConv      // package type convert
	curFile    *HeaderFile    // current processing c header file.
	incomplete map[string]*gogen.TypeDecl
}

type PackageConfig struct {
	PkgPath     string
	Name        string
	OutputDir   string
	SymbolTable *cfg.SymbolTable
	GenConf     *gogen.Config
	CppgConf    *cppgtypes.Config
	Public      map[string]string
}

func (p *PackageConfig) GetGoName(name string, inCurPkg bool) string {
	goName, ok := p.Public[name]
	if ok {
		return goName
	}
	if inCurPkg {
		name = names.RemovePrefixedName(name, p.CppgConf.TrimPrefixes)
	}
	return names.CPubName(name)
}

func (p *PackageConfig) GetIncPaths() ([]string, error) {
	cflags := cfgparse.ParseCFlags(p.CppgConf.CFlags)
	incPaths, _, err := cflags.GenHeaderFilePaths(p.CppgConf.Include)
	return incPaths, err
}

// When creating a new package for conversion, a Go file named after the package is generated by default.
// If SetCurFile is not called, all type conversions will be written to this default Go file.
func NewPackage(config *PackageConfig) *Package {
	p := &Package{
		p:          gogen.NewPackage(config.PkgPath, config.Name, config.GenConf),
		name:       config.Name,
		conf:       config,
		incomplete: make(map[string]*gogen.TypeDecl),
	}
	clib := p.p.Import("github.com/goplus/llgo/c")
	typeMap := NewBuiltinTypeMapWithPkgRefS(clib, p.p.Unsafe())
	p.cvt = NewConv(&TypeConfig{
		Types:       p.p.Types,
		TypeMap:     typeMap,
		SymbolTable: config.SymbolTable,
		Package:     p,
	})
	p.SetCurFile(p.Name(), "", false, false, false)
	return p
}

// get current pkg's include file
func (p *Package) GetIncPaths() []string {
	incPaths, err := p.conf.GetIncPaths()
	if err != nil {
		log.Println("failed to gen include paths: \n", err.Error())
	}
	return incPaths
}

func (p *Package) SetCurFile(file string, incPath string, isHeaderFile bool, inCurPkg bool, isSys bool) error {
	curHeaderFile, err := NewHeaderFile(file, incPath, isHeaderFile, inCurPkg, isSys)
	if err != nil {
		return err
	}
	p.curFile = curHeaderFile
	fileName := p.curFile.ToGoFileName()
	if debug {
		log.Printf("SetCurFile: %s File in Current Package: %v\n", fileName, inCurPkg)
	}
	if _, err := p.p.SetCurFile(fileName, true); err != nil {
		return fmt.Errorf("fail to set current file %s\n%w", file, err)
	}
	p.p.Unsafe().MarkForceUsed(p.p)
	return nil
}

func (p *Package) GetGenPackage() *gogen.Package {
	return p.p
}

func (p *Package) GetOutputDir() string {
	return p.conf.OutputDir
}

func (p *Package) Name() string {
	return p.name
}

func (p *Package) GetTypeConv() *TypeConv {
	return p.cvt
}

// todo(zzy):refine logic
func (p *Package) linkLib(lib string) error {
	if lib == "" {
		return fmt.Errorf("empty lib name")
	}
	linkString := fmt.Sprintf("link: %s;", lib)
	p.p.CB().NewConstStart(types.Typ[types.String], "LLGoPackage").Val(linkString).EndInit(1)
	return nil
}

func (p *Package) NewFuncDecl(funcDecl *ast.FuncDecl) error {
	skip, anony, err := p.cvt.handleSysType(funcDecl.Name, funcDecl.Loc, p.curFile.sysIncPath)
	if skip {
		if debug {
			log.Printf("NewFuncDecl: %v is a function of system header file\n", funcDecl.Name)
		}
		return err
	}
	if debug {
		log.Printf("NewFuncDecl: %v\n", funcDecl.Name)
	}
	if anony {
		return fmt.Errorf("anonymous function not supported")
	}

	goFuncName, err := p.cvt.LookupSymbol(cfg.MangleNameType(funcDecl.MangledName))
	if err != nil {
		// not gen the function not in the symbolmap
		return err
	}
	if obj := p.p.Types.Scope().Lookup(goFuncName); obj != nil {
		return fmt.Errorf("function %s already defined", goFuncName)
	}
	sig, err := p.cvt.ToSignature(funcDecl.Type)
	if err != nil {
		return err
	}
	decl := p.p.NewFuncDecl(token.NoPos, string(goFuncName), sig)
	doc := CommentGroup(funcDecl.Doc)
	doc.AddCommentGroup(NewFuncDocComments(funcDecl.Name.Name, string(goFuncName)))
	decl.SetComments(p.p, doc.CommentGroup)
	return nil
}

// NewTypeDecl converts C/C++ type declarations to Go.
// Besides regular type declarations, it also supports:
// - Forward declarations: Pre-registers incomplete types for later definition
// - Self-referential types: Handles types that reference themselves (like linked lists)
func (p *Package) NewTypeDecl(typeDecl *ast.TypeDecl) error {
	skip, anony, err := p.cvt.handleSysType(typeDecl.Name, typeDecl.Loc, p.curFile.sysIncPath)
	if skip {
		if debug {
			log.Printf("NewTypeDecl: %s type of system header\n", typeDecl.Name)
		}
		return err
	}
	if debug {
		log.Printf("NewTypeDecl: %v\n", typeDecl.Name)
	}
	if anony {
		if debug {
			log.Println("NewTypeDecl:Skip a anonymous type")
		}
		return nil
	}

	// every type name should be public
	name, changed, err := p.DeclName(typeDecl.Name.Name, true)
	if err != nil {
		return err
	}

	decl := p.handleTypeDecl(name, typeDecl, changed)

	if !p.cvt.inComplete(typeDecl.Type) {
		if err := p.handleCompleteType(decl, typeDecl.Type, name); err != nil {
			return err
		}
	}
	return nil
}

// handleTypeDecl creates a new type declaration or retrieves existing one
func (p *Package) handleTypeDecl(name string, typeDecl *ast.TypeDecl, changed bool) *gogen.TypeDecl {
	if existDecl, exists := p.incomplete[name]; exists {
		return existDecl
	}
	decl := p.emptyTypeDecl(name, typeDecl.Doc)
	if p.cvt.inComplete(typeDecl.Type) {
		p.incomplete[name] = decl
	}
	if changed {
		substObj(p.p.Types, p.p.Types.Scope(), typeDecl.Name.Name, decl.Type().Obj())
	}
	return decl
}

func (p *Package) handleCompleteType(decl *gogen.TypeDecl, typ *ast.RecordType, name string) error {
	structType, err := p.cvt.RecordTypeToStruct(typ)
	if err != nil {
		decl.Delete()
		return err
	}
	decl.InitType(p.p, structType)
	delete(p.incomplete, name)
	return nil
}

func (p *Package) emptyTypeDecl(name string, doc *ast.CommentGroup) *gogen.TypeDecl {
	typeBlock := p.p.NewTypeDefs()
	typeBlock.SetComments(CommentGroup(doc).CommentGroup)
	return typeBlock.NewType(name)
}

func (p *Package) NewTypedefDecl(typedefDecl *ast.TypedefDecl) error {
	skip, _, err := p.cvt.handleSysType(typedefDecl.Name, typedefDecl.Loc, p.curFile.sysIncPath)
	if skip {
		if debug {
			log.Printf("NewTypedefDecl: %v is a typedef of system header file\n", typedefDecl.Name)
		}
		return err
	}
	if debug {
		log.Printf("NewTypedefDecl: %v\n", typedefDecl.Name)
	}
	name, changed, err := p.DeclName(typedefDecl.Name.Name, true)
	if err != nil {
		return err
	}
	// todo(zzy): this block will be removed after https://github.com/goplus/llgo/pull/870
	if obj := p.p.Types.Scope().Lookup(name); obj != nil {
		// for a typedef ,always appear same name like
		// typedef struct foo { int a; } foo;
		// For this typedef, we only need skip this
		return nil
	}

	genDecl := p.p.NewTypeDefs()
	typ, err := p.ToType(typedefDecl.Type)
	if err != nil {
		return err
	}
	typeSpecdecl := genDecl.NewType(name)
	typeSpecdecl.InitType(p.p, typ)
	if _, ok := typ.(*types.Signature); ok {
		genDecl.SetComments(NewTypecDocComments())
	}
	if changed {
		substObj(p.p.Types, p.p.Types.Scope(), typedefDecl.Name.Name, typeSpecdecl.Type().Obj())
	}
	return nil
}

// Convert ast.Expr to types.Type
func (p *Package) ToType(expr ast.Expr) (types.Type, error) {
	return p.cvt.ToType(expr)
}

func (p *Package) NewTypedefs(name string, typ types.Type) *gogen.TypeDecl {
	def := p.p.NewTypeDefs()
	t := def.NewType(name)
	t.InitType(def.Pkg(), typ)
	def.Complete()
	return t
}

func (p *Package) NewEnumTypeDecl(enumTypeDecl *ast.EnumTypeDecl) error {
	skip, _, err := p.cvt.handleSysType(enumTypeDecl.Name, enumTypeDecl.Loc, p.curFile.sysIncPath)
	if skip {
		if debug {
			log.Printf("NewEnumTypeDecl: %v is a enum type of system header file\n", enumTypeDecl.Name)
		}
		return err
	}
	if debug {
		log.Printf("NewEnumTypeDecl: %v\n", enumTypeDecl.Name)
	}
	enumType, enumTypeName, err := p.createEnumType(enumTypeDecl.Name)
	if err != nil {
		return err
	}
	if len(enumTypeDecl.Type.Items) > 0 {
		err = p.createEnumItems(enumTypeDecl.Type.Items, enumType, enumTypeName)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Package) createEnumType(enumName *ast.Ident) (types.Type, string, error) {
	var name string
	var changed bool
	var err error
	var t *gogen.TypeDecl
	if enumName != nil {
		name, changed, err = p.DeclName(enumName.Name, true)
		if err != nil {
			return nil, "", fmt.Errorf("enum type %s already defined", enumName.Name)
		}
	}
	enumType := p.cvt.ToDefaultEnumType()
	if name != "" {
		t = p.NewTypedefs(name, enumType)
		enumType = p.p.Types.Scope().Lookup(name).Type()
	}
	if changed {
		substObj(p.p.Types, p.p.Types.Scope(), enumName.Name, t.Type().Obj())
	}
	return enumType, name, nil
}

func (p *Package) createEnumItems(items []*ast.EnumItem, enumType types.Type, enumTypeName string) error {
	constDefs := p.p.NewConstDefs(p.p.Types.Scope())
	for _, item := range items {
		var constName string
		// maybe get a new name,because the after executed name,have some situation will found same name
		if enumTypeName != "" {
			constName = enumTypeName + "_" + item.Name.Name
		} else {
			constName = item.Name.Name
		}
		name, changed, err := p.DeclName(constName, false)
		if err != nil {
			return fmt.Errorf("enum item %s already defined %w", name, err)
		}
		val, err := Expr(item.Value).ToInt()
		if err != nil {
			return err
		}
		constDefs.New(func(cb *gogen.CodeBuilder) int {
			cb.Val(val)
			return 1
		}, 0, token.NoPos, enumType, name)
		if changed {
			if obj := p.p.Types.Scope().Lookup(name); obj != nil {
				substObj(p.p.Types, p.p.Types.Scope(), item.Name.Name, obj)
			}
		}
	}
	return nil
}

// Write generates a Go file based on the package content.
// The output file will be generated in a subdirectory named after the package within the outputDir.
// If outputDir is not provided, the current directory will be used.
// The header file name is the go file name.
//
// Files that are already processed in dependent packages will not be output.
func (p *Package) Write(headerFile string) error {
	if p.curFile.isSys {
		return nil
	}
	fileName := names.HeaderFileToGo(headerFile)
	filePath := filepath.Join(p.GetOutputDir(), fileName)
	if debug {
		log.Printf("Write HeaderFile [%s] from  gogen:[%s] to [%s]\n", headerFile, fileName, filePath)
	}
	return p.writeToFile(fileName, filePath)
}

func (p *Package) WriteLinkFile() (string, error) {
	fileName := p.name + "_autogen_link.go"
	filePath := filepath.Join(p.GetOutputDir(), fileName)
	p.p.SetCurFile(fileName, true)
	err := p.linkLib(p.conf.CppgConf.Libs)
	if debug {
		log.Printf("Write LinkFile [%s] from  gogen:[%s] to [%s]\n", fileName, fileName, filePath)
	}
	if err != nil {
		return "", fmt.Errorf("failed to link lib: %w", err)
	}
	if err := p.writeToFile(fileName, filePath); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}
	return filePath, nil
}

// WriteDefaultFileToBuffer writes the content of the default Go file to a buffer.
// The default file is named after the package (p.Name() + ".go").
// This method is particularly useful for testing type outputs, especially in package tests
// where there typically isn't (and doesn't need to be) a corresponding header file.
// Before calling SetCurFile, all type creations are written to this default gogen file.
// It allows for easy inspection of generated types without the need for actual file I/O.
func (p *Package) WriteDefaultFileToBuffer() (*bytes.Buffer, error) {
	return p.WriteToBuffer(p.Name() + ".go")
}

// Write the corresponding files in gogen package to the file
func (p *Package) writeToFile(genFName string, filePath string) error {
	buf, err := p.WriteToBuffer(genFName)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, buf.Bytes(), 0644)
}

// Write the corresponding files in gogen package to the buffer
func (p *Package) WriteToBuffer(genFName string) (*bytes.Buffer, error) {
	for _, decl := range p.incomplete {
		decl.InitType(p.p, types.NewStruct(p.cvt.defaultRecordField(), nil))
	}
	p.incomplete = make(map[string]*gogen.TypeDecl, 0)
	buf := new(bytes.Buffer)
	err := p.p.WriteTo(buf, genFName)
	if err != nil {
		return nil, fmt.Errorf("failed to write to buffer: %w", err)
	}
	return buf, nil
}

func (p *Package) WritePubFile() error {
	return cfg.WritePubFile(filepath.Join(p.GetOutputDir(), "llcppg.pub"), p.conf.Public)
}

func (p *Package) getAllDepPkgs(deps []*deps.CPackage) []string {
	allDepIncs := make([]string, 0)
	scope := p.p.Types.Scope()
	for _, dep := range deps {
		allDepIncs = append(allDepIncs, dep.StdIncs...)
		depPkg := p.p.Import(dep.Path)
		for cName, pubGoName := range dep.Pubs {
			if pubGoName == "" {
				pubGoName = cName
			}
			if obj := depPkg.TryRef(pubGoName); obj != nil {
				var preObj types.Object
				if pubGoName == cName {
					preObj = obj
				} else {
					preObj = gogen.NewSubst(token.NoPos, p.p.Types, cName, obj)
				}
				if old := scope.Insert(preObj); old != nil {
					log.Printf("conflicted name `%v` in %v, previous definition is %v\n", pubGoName, dep.Path, old)
				}
			}
		}
	}
	return allDepIncs
}

// For a decl name, if it's a current package, remove the prefixed name
// For a decl name, it should be unique
// todo(zzy): not current converter package file,need not remove prefixed name
func (p *Package) DeclName(name string, collect bool) (pubName string, changed bool, err error) {
	originName := name
	name = p.conf.GetGoName(name, p.curFile.inCurPkg)
	// if the type is incomplete,it's ok to have the same name
	if obj := p.p.Types.Scope().Lookup(name); obj != nil && p.incomplete[name] == nil {
		return "", false, fmt.Errorf("type %s already defined,original name is %s", name, originName)
	}
	changed = name != originName
	if collect && p.curFile.inCurPkg {
		if changed {
			p.conf.Public[originName] = name
		} else {
			p.conf.Public[originName] = ""
		}
	}
	return name, changed, nil
}

// AllDepIncs returns all std include paths of dependent packages
func (p *Package) AllDepIncs() []string {
	deps, err := deps.LoadDeps(p.conf.OutputDir, p.conf.CppgConf.Deps)
	if err != nil {
		log.Println("failed to load deps: \n", err.Error())
	}
	return p.getAllDepPkgs(deps)
}

type PkgMapping struct {
	Pattern string
	Package string
}

const (
	LLGO_C       = "github.com/goplus/llgo/c"
	LLGO_SYSTEM  = "github.com/goplus/llgo/c/system"
	LLGO_TIME    = "github.com/goplus/llgo/c/time"
	LLGO_MATH    = "github.com/goplus/llgo/c/math"
	LLGO_I18N    = "github.com/goplus/llgo/c/i18n"
	LLGO_COMPLEX = "github.com/goplus/llgo/c/math/cmplx"

	LLGO_PTHREAD  = "github.com/goplus/llgo/c/pthread"
	LLGO_UNIX_NET = "github.com/goplus/llgo/c/unix/net"
)

// IncPathToPkg determines the Go package for a given C include path.
//
// According to the C language specification, when including a standard library,
// such as stdio.h, certain declarations must be provided (e.g., FILE type).
// However, these types don't have to be declared in the header file itself.
// On MacOS, for example, the actual declaration exists in _stdio.h. Therefore,
// each standard library header file can be viewed as defining an interface,
// independent of its implementation.
//
// In our current requirements, the matching follows this order:
//  1. First match standard library interface headers (like stdio.h, stdint.h)
//     which define required types and functions
//  2. Then match implementation headers (like _stdio.h, sys/_types/_int8_t.h)
//     which contain the actual type definitions
//
// For example:
// - stdio.h as interface, specifies that FILE type must be provided
// - _stdio.h as implementation, provides the actual FILE definition on MacOS
func IncPathToPkg(incPath string) (pkg string, isDefault bool) {
	pkgMappings := []PkgMapping{
		// c std
		{Pattern: `(^|[^a-zA-Z0-9])stdint[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])stddef[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])stdio[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])stdlib[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])string[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])stdbool[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])stdarg[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])limits[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])ctype[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])uchar[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])wchar[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])wctype[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])inttypes[^a-zA-Z0-9]`, Package: LLGO_C},

		{Pattern: `(^|[^a-zA-Z0-9])signal[^a-zA-Z0-9]`, Package: LLGO_SYSTEM},
		{Pattern: `(^|[^a-zA-Z0-9])setjmp[^a-zA-Z0-9]`, Package: LLGO_SYSTEM},
		{Pattern: `(^|[^a-zA-Z0-9])assert[^a-zA-Z0-9]`, Package: LLGO_SYSTEM},
		{Pattern: `(^|[^a-zA-Z0-9])stdalign[^a-zA-Z0-9]`, Package: LLGO_SYSTEM},

		{Pattern: `(^|[^a-zA-Z0-9])math[^a-zA-Z0-9]`, Package: LLGO_MATH},
		{Pattern: `(^|[^a-zA-Z0-9])fenv[^a-zA-Z0-9]`, Package: LLGO_MATH},
		{Pattern: `(^|[^a-zA-Z0-9])complex[^a-zA-Z0-9]`, Package: LLGO_COMPLEX},

		{Pattern: `(^|[^a-zA-Z0-9])time[^a-zA-Z0-9]`, Package: LLGO_TIME},

		{Pattern: `(^|[^a-zA-Z0-9])pthread[^a-zA-Z0-9]`, Package: LLGO_PTHREAD},

		{Pattern: `(^|[^a-zA-Z0-9])locale[^a-zA-Z0-9]`, Package: LLGO_I18N},

		//c posix
		{Pattern: `(^|[^a-zA-Z0-9])socket[^a-zA-Z0-9]`, Package: LLGO_UNIX_NET},
		{Pattern: `(^|[^a-zA-Z0-9])arpa[^a-zA-Z0-9]`, Package: LLGO_UNIX_NET},
		{Pattern: `(^|[^a-zA-Z0-9])netinet6?[^a-zA-Z0-9]`, Package: LLGO_UNIX_NET},
		{Pattern: `(^|[^a-zA-Z0-9])net[^a-zA-Z0-9]`, Package: LLGO_UNIX_NET},

		// impl file
		{Pattern: `_int\d+_t`, Package: LLGO_C},
		{Pattern: `_uint\d+_t`, Package: LLGO_C},
		{Pattern: `_size_t`, Package: LLGO_C},
		{Pattern: `_intptr_t`, Package: LLGO_C},
		{Pattern: `_uintptr_t`, Package: LLGO_C},
		{Pattern: `_ptrdiff_t`, Package: LLGO_C},

		{Pattern: `malloc`, Package: LLGO_C},
		{Pattern: `alloc`, Package: LLGO_C},

		{Pattern: `(^|[^a-zA-Z0-9])clock_t[^a-zA-Z0-9]`, Package: LLGO_TIME},
		{Pattern: `(^|[^a-zA-Z0-9])tm[^a-zA-Z0-9]`, Package: LLGO_TIME},

		// before must the special type.h such as _pthread_types.h ....
		{Pattern: `\w+_t[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])types[^a-zA-Z0-9]`, Package: LLGO_C},
		{Pattern: `(^|[^a-zA-Z0-9])sys[^a-zA-Z0-9]`, Package: LLGO_SYSTEM},

		// {Pattern: `(^|[^a-zA-Z0-9])strings\.h$`, Package: LLGO_C},
	}

	for _, mapping := range pkgMappings {
		matched, err := regexp.MatchString(mapping.Pattern, incPath)
		if err != nil {
			panic(err)
		}
		if matched {
			return mapping.Package, false
		}
	}
	return LLGO_C, true
}
