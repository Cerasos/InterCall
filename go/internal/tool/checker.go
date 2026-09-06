package tool

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/tools/go/packages"
)

// This file implements the generated-Go type checking of SPEC.md
// "One-file ownership and safe replacement" (RM-00 decision 9): the
// complete generated import and export binding is parsed and
// go/types-checked in memory before any filesystem mutation. Export
// checking reuses the exact *types.Package identities of the one
// combined discovery load for the provider, exception, and reachable
// named-type packages and for every standard-library package the load
// contains; import checking uses one synthetic root-runtime SPI model,
// guarded by the durable parity test of checker_test.go, which compares
// every modeled exported generated-code bridge object and signature
// with the actual root package. Only discovery uses go/packages, no
// subprocess compiles anything, and the standard library resolves from
// the toolchain source tree, so checking is fully in-memory and
// hermetic.
//
// The checker is the final validation layer before the artifact write:
// namespace reservation and the projection preflight diagnose every
// user-triggerable defect first, and the checker proves the generated
// bytes are valid Go — complete function bodies included — against the
// exact identities the generated code must compile against. A checker
// diagnostic names the logical binding target and the physical position
// inside the generated file, never an absolute, resolved, or staging
// path.

// runtimeImportPath is the import path of the root runtime package,
// whose exported surface the synthetic SPI model mirrors.
const runtimeImportPath = "github.com/cerasos/intercall/go"

// GoChecker verifies one complete generated binding in memory.
//
// CheckGo parses and go/types-checks the exact binding bytes the
// artifact layer is about to write — the formatted complete file
// including the ownership lines — and returns a diagnostic that names
// logical and the physical position of the first type error. It never
// touches the filesystem and never runs a subprocess.
type GoChecker interface {
	CheckGo(logical string, src []byte) error
}

// NewImportGoChecker returns the checker of one generated import
// binding: the synthetic runtime SPI model resolves the runtime import
// path, and every standard-library import resolves from the toolchain
// source tree with the same package instances the model was built
// against, so the model's context.Context is the very package instance
// the generated callers import.
func NewImportGoChecker() GoChecker {
	return &importGoChecker{imp: newBindingImporter(nil)}
}

// NewExportGoChecker returns the checker of one generated export
// binding over one discovery result.
//
// The checker's resolution table holds the exact *types.Package
// identities of the one combined discovery load — the transitive import
// closure of the explicit packages — so the generated binding's context
// import is the very package instance the provider signatures were
// mapped against, and the provider, application exception, and
// reachable named-type packages keep their loaded identities. When the
// load does not contain the root runtime package, the synthetic runtime
// SPI model resolves it, exactly as in the import checker; standard
// library packages outside the load resolve from the toolchain source
// tree. res may be nil, which yields an empty identity table.
func NewExportGoChecker(res *DiscoverResult) GoChecker {
	return &exportGoChecker{imp: newBindingImporter(res)}
}

// importGoChecker is the import binding checker.
type importGoChecker struct {
	imp *bindingImporter
}

// CheckGo implements GoChecker.
func (c *importGoChecker) CheckGo(logical string, src []byte) error {
	return checkGo(logical, src, c.imp)
}

// exportGoChecker is the export binding checker.
type exportGoChecker struct {
	imp *bindingImporter
}

// CheckGo implements GoChecker.
func (c *exportGoChecker) CheckGo(logical string, src []byte) error {
	return checkGo(logical, src, c.imp)
}

// bindingImporter resolves the imports of one generated binding: the
// discovery-load identities first, then the synthetic runtime SPI
// model for the runtime import path, then the standard library source
// tree. Every resolution is cached, so the model, the checked file, and
// every standard-library package share one instance per package.
type bindingImporter struct {
	graph map[string]*types.Package // discovery identities, path -> package
	std   *stdlibImporter           // standard library source resolution
	cache map[string]*types.Package

	spi     *types.Package // synthetic runtime SPI model
	spiErr  error
	spiOnce sync.Once
}

// newBindingImporter builds the importer of one checker. res carries
// the discovery identities; nil yields an empty identity table.
func newBindingImporter(res *DiscoverResult) *bindingImporter {
	imp := &bindingImporter{
		graph: make(map[string]*types.Package),
		std:   newStdlibImporter(),
		cache: make(map[string]*types.Package),
	}
	if res != nil {
		for _, p := range res.Packages {
			indexTypes(p.pkg, imp.graph)
		}
	}
	return imp
}

// indexTypes records pkg and its transitive import closure in idx,
// keeping the exact *types.Package identities of the one discovery
// load.
func indexTypes(pkg *packages.Package, idx map[string]*types.Package) {
	if pkg == nil || idx[pkg.PkgPath] != nil {
		return
	}
	if pkg.Types != nil {
		idx[pkg.PkgPath] = pkg.Types
	}
	for _, dep := range pkg.Imports {
		indexTypes(dep, idx)
	}
}

// Import implements types.Importer.
func (imp *bindingImporter) Import(path string) (*types.Package, error) {
	if pkg := imp.cache[path]; pkg != nil {
		return pkg, nil
	}
	if pkg := imp.graph[path]; pkg != nil {
		imp.cache[path] = pkg
		return pkg, nil
	}
	if path == runtimeImportPath {
		imp.spiOnce.Do(func() {
			imp.spi, imp.spiErr = buildRuntimeSPIModel(imp)
		})
		if imp.spiErr != nil {
			return nil, imp.spiErr
		}
		imp.cache[path] = imp.spi
		return imp.spi, nil
	}
	pkg, err := imp.std.Import(path)
	if err != nil {
		return nil, err
	}
	imp.cache[path] = pkg
	return pkg, nil
}

// checkGo parses and type-checks one complete generated binding source
// in memory and converts the first failure into a diagnostic at its
// exact physical position in the generated file.
//
// The logical path is the binding target's logical operand path; the
// diagnostic never reports an absolute, resolved, or staging path, and
// the go/types error text carries no file position prefix of its own.
func checkGo(logical string, src []byte, imp types.Importer) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, logical, src, parser.AllErrors)
	if err != nil {
		return &Error{
			Filename: logical,
			Pos:      Position{Line: 1, Column: 1},
			Msg:      fmt.Sprintf("the generated binding does not parse: %v", err),
		}
	}
	if f.Name == nil {
		return &Error{
			Filename: logical,
			Pos:      Position{Line: 1, Column: 1},
			Msg:      "the generated binding has no package clause",
		}
	}
	conf := types.Config{Importer: imp}
	if _, err := conf.Check(f.Name.Name, fset, []*ast.File{f}, nil); err != nil {
		var te types.Error
		if !errors.As(err, &te) {
			return &Error{
				Filename: logical,
				Pos:      Position{Line: 1, Column: 1},
				Msg:      fmt.Sprintf("the generated binding does not type-check: %v", err),
			}
		}
		pos := fset.PositionFor(te.Pos, false)
		return &Error{
			Filename: logical,
			Pos:      Position{Offset: pos.Offset, Line: pos.Line, Column: pos.Column},
			Msg:      fmt.Sprintf("the generated binding does not type-check: %s", te.Msg),
		}
	}
	return nil
}

// stdlibImporter resolves standard-library packages from the toolchain
// source tree, so generated-binding checking never runs a subprocess
// and never uses go/packages outside discovery. The GOROOT root is the
// GOROOT environment value when set, and otherwise the toolchain that
// built the tool, exactly as the go command resolves it. Resolved
// packages are cached per root and import path, so repeated checker
// constructions in one process share one instance per standard-library
// package.
type stdlibImporter struct {
	ctxt  build.Context
	fset  *token.FileSet
	cache map[string]*types.Package
}

// stdlibCache shares one complete *types.Package per (GOROOT root,
// import path) across checker constructions. go/types packages are
// immutable after checking, so the sharing is safe.
var stdlibCache sync.Map // "root\x00path" -> *types.Package

// newStdlibImporter builds one standard-library source importer.
func newStdlibImporter() *stdlibImporter {
	ctxt := build.Default
	if root := os.Getenv("GOROOT"); root != "" {
		ctxt.GOROOT = root
	}
	return &stdlibImporter{ctxt: ctxt, fset: token.NewFileSet(), cache: make(map[string]*types.Package)}
}

// Import implements types.Importer.
func (imp *stdlibImporter) Import(path string) (*types.Package, error) {
	if pkg := imp.cache[path]; pkg != nil {
		return pkg, nil
	}
	if path == "unsafe" {
		imp.cache[path] = types.Unsafe
		return types.Unsafe, nil
	}
	key := imp.ctxt.GOROOT + "\x00" + path
	if cached, ok := stdlibCache.Load(key); ok {
		pkg := cached.(*types.Package)
		imp.cache[path] = pkg
		return pkg, nil
	}
	bp, err := imp.ctxt.Import(path, "", 0)
	if err != nil {
		return nil, fmt.Errorf("standard library package %q is not available: %v", path, importCause(err))
	}
	if len(bp.CgoFiles) > 0 {
		return nil, fmt.Errorf("standard library package %q uses cgo and cannot be resolved from source", path)
	}
	var afs []*ast.File
	for _, name := range bp.GoFiles {
		src, err := os.ReadFile(filepath.Join(bp.Dir, name))
		if err != nil {
			return nil, fmt.Errorf("standard library package %q: %v", path, importCause(err))
		}
		// The logical file name is the package-relative path, so a
		// diagnostic can never expose the GOROOT source directory.
		af, err := parser.ParseFile(imp.fset, path+"/"+name, src, parser.AllErrors)
		if err != nil {
			return nil, fmt.Errorf("standard library package %q does not parse: %v", path, err)
		}
		afs = append(afs, af)
	}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(bp.ImportPath, imp.fset, afs, nil)
	if err != nil {
		return nil, fmt.Errorf("standard library package %q does not type-check: %v", path, err)
	}
	imp.cache[path] = pkg
	stdlibCache.Store(key, pkg)
	return pkg, nil
}

// importCause renders the underlying cause of one standard-library
// resolution failure without any path: a *fs.PathError or *os.LinkError
// contributes only its Err text, and every other failure class renders
// as a fixed message, so a checker diagnostic can never expose the
// GOROOT root or another absolute path.
func importCause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) && le.Err != nil {
		return le.Err.Error()
	}
	return "not found"
}

// buildRuntimeSPIModel builds the synthetic runtime SPI package: the
// complete exported object surface of the root runtime package that
// generated bindings compile against. The model's standard-library
// references resolve through imp, so its context.Context and io
// interfaces are the very package instances the generated binding's
// imports resolve to — the identity requirement that makes the checked
// callers type-check against the model.
func buildRuntimeSPIModel(imp types.Importer) (*types.Package, error) {
	ctxPkg, err := imp.Import("context")
	if err != nil {
		return nil, fmt.Errorf("resolving the standard library context package for the runtime SPI model: %v", err)
	}
	ioPkg, err := imp.Import("io")
	if err != nil {
		return nil, fmt.Errorf("resolving the standard library io package for the runtime SPI model: %v", err)
	}

	pkg := types.NewPackage(runtimeImportPath, "intercall")
	scope := pkg.Scope()
	noPos := token.NoPos
	errType := types.Universe.Lookup("error").Type()
	byteType := types.Typ[types.Byte]
	uint64Type := types.Typ[types.Uint64]
	ctxType := ctxPkg.Scope().Lookup("Context").Type()
	readerType := ioPkg.Scope().Lookup("Reader").Type()
	writerType := ioPkg.Scope().Lookup("Writer").Type()
	closerType := ioPkg.Scope().Lookup("Closer").Type()

	// insert declares one object in the model's package scope.
	insert := func(o types.Object) {
		if scope.Insert(o) != nil {
			panic("tool: internal error: duplicate object in the runtime SPI model: " + o.Name())
		}
	}

	// newType declares one named type with the given underlying type.
	newType := func(name string, underlying types.Type) *types.TypeName {
		tn := types.NewTypeName(noPos, pkg, name, nil)
		types.NewNamed(tn, underlying, nil)
		insert(tn)
		return tn
	}

	byteStreamIface := types.NewInterfaceType(nil, []types.Type{readerType, writerType, closerType})
	byteStreamIface.Complete()
	byteStream := newType("ByteStream", byteStreamIface).Type()

	dispatchSig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewVar(noPos, pkg, "ctx", ctxType),
			types.NewVar(noPos, pkg, "key", uint64Type),
			types.NewVar(noPos, pkg, "payload", types.NewSlice(byteType)),
		),
		types.NewTuple(
			types.NewVar(noPos, pkg, "key", uint64Type),
			types.NewVar(noPos, pkg, "payload", types.NewSlice(byteType)),
		), false)
	dispatch := newType("Dispatch", dispatchSig).Type()

	requestEncoder := newType("RequestEncoder", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(),
		types.NewTuple(
			types.NewVar(noPos, pkg, "", types.NewSlice(byteType)),
			types.NewVar(noPos, pkg, "", errType),
		), false)).Type()

	responseDecoder := newType("ResponseDecoder", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewVar(noPos, pkg, "key", uint64Type),
			types.NewVar(noPos, pkg, "payload", types.NewSlice(byteType)),
		),
		types.NewTuple(types.NewVar(noPos, pkg, "", errType)), false)).Type()

	interfaceID := newType("InterfaceID", types.NewArray(byteType, 32)).Type()

	exportState := newType("exportState", types.NewStruct([]*types.Var{
		types.NewField(noPos, pkg, "dispatch", dispatch, false),
		types.NewField(noPos, pkg, "interfaceID", interfaceID, false),
		types.NewField(noPos, pkg, "hasInterfaceID", types.Typ[types.Bool], false),
		types.NewField(noPos, pkg, "identity", byteType, false),
	}, nil)).Type()
	exportBindingTN := newType("ExportBinding", types.NewStruct([]*types.Var{
		types.NewField(noPos, pkg, "state", types.NewPointer(exportState), false),
	}, nil))
	exportBinding := exportBindingTN.Type()

	importState := newType("importState", types.NewStruct([]*types.Var{
		types.NewField(noPos, pkg, "interfaceID", interfaceID, false),
		types.NewField(noPos, pkg, "hasInterfaceID", types.Typ[types.Bool], false),
		types.NewField(noPos, pkg, "identity", byteType, false),
	}, nil)).Type()
	importBindingTN := newType("ImportBinding", types.NewStruct([]*types.Var{
		types.NewField(noPos, pkg, "state", types.NewPointer(importState), false),
	}, nil))
	importBinding := importBindingTN.Type()

	addInterfaceIDMethod := func(named *types.Named) {
		sig := types.NewSignatureType(types.NewVar(noPos, pkg, "b", named), nil, nil,
			types.NewTuple(),
			types.NewTuple(
				types.NewVar(noPos, pkg, "", interfaceID),
				types.NewVar(noPos, pkg, "", types.Typ[types.Bool]),
			), false)
		named.AddMethod(types.NewFunc(noPos, pkg, "InterfaceID", sig))
	}
	addInterfaceIDMethod(exportBindingTN.Type().(*types.Named))
	addInterfaceIDMethod(importBindingTN.Type().(*types.Named))

	connectionTN := newType("Connection", types.NewStruct(nil, nil))
	connection := connectionTN.Type().(*types.Named)
	addMethod := func(name string, params, results *types.Tuple) {
		sig := types.NewSignatureType(types.NewVar(noPos, pkg, "c", types.NewPointer(connection)), nil, nil, params, results, false)
		connection.AddMethod(types.NewFunc(noPos, pkg, name, sig))
	}
	addMethod("Call",
		types.NewTuple(
			types.NewVar(noPos, pkg, "ctx", ctxType),
			types.NewVar(noPos, pkg, "imp", importBinding),
			types.NewVar(noPos, pkg, "procedureKey", uint64Type),
			types.NewVar(noPos, pkg, "encode", requestEncoder),
			types.NewVar(noPos, pkg, "decode", responseDecoder),
		),
		types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	addMethod("Close", types.NewTuple(), types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	addMethod("Wait", types.NewTuple(), types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	addMethod("WaitForHandlers", types.NewTuple(), types.NewTuple(types.NewVar(noPos, pkg, "", errType)))

	insert(types.NewFunc(noPos, pkg, "NewExportBinding", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(types.NewVar(noPos, pkg, "dispatch", dispatch)),
		types.NewTuple(
			types.NewVar(noPos, pkg, "", exportBinding),
			types.NewVar(noPos, pkg, "", errType),
		), false)))
	insert(types.NewFunc(noPos, pkg, "NewExportBindingWithInterfaceID", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewVar(noPos, pkg, "dispatch", dispatch),
			types.NewVar(noPos, pkg, "id", interfaceID),
		),
		types.NewTuple(
			types.NewVar(noPos, pkg, "", exportBinding),
			types.NewVar(noPos, pkg, "", errType),
		), false)))
	insert(types.NewFunc(noPos, pkg, "NewImportBinding", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(),
		types.NewTuple(types.NewVar(noPos, pkg, "", importBinding)), false)))
	insert(types.NewFunc(noPos, pkg, "NewImportBindingWithInterfaceID", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(types.NewVar(noPos, pkg, "id", interfaceID)),
		types.NewTuple(types.NewVar(noPos, pkg, "", importBinding)), false)))
	insert(types.NewFunc(noPos, pkg, "EmptyExportBinding", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(),
		types.NewTuple(types.NewVar(noPos, pkg, "", exportBinding)), false)))
	insert(types.NewFunc(noPos, pkg, "EmptyImportBinding", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(),
		types.NewTuple(types.NewVar(noPos, pkg, "", importBinding)), false)))
	newConnectionSig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewVar(noPos, pkg, "ctx", ctxType),
			types.NewVar(noPos, pkg, "stream", byteStream),
			types.NewVar(noPos, pkg, "export", exportBinding),
			types.NewVar(noPos, pkg, "imp", importBinding),
		),
		types.NewTuple(
			types.NewVar(noPos, pkg, "", types.NewPointer(connection)),
			types.NewVar(noPos, pkg, "", errType),
		), false)
	insert(types.NewFunc(noPos, pkg, "NewConnection", newConnectionSig))
	insert(types.NewFunc(noPos, pkg, "NewNegotiatedClientConnection", newConnectionSig))
	insert(types.NewFunc(noPos, pkg, "NewNegotiatedServerConnection", newConnectionSig))
	insert(types.NewFunc(noPos, pkg, "WithConnection", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(
			types.NewVar(noPos, pkg, "ctx", ctxType),
			types.NewVar(noPos, pkg, "conn", types.NewPointer(connection)),
		),
		types.NewTuple(types.NewVar(noPos, pkg, "", ctxType)), false)))
	insert(types.NewFunc(noPos, pkg, "ConnectionFromContext", types.NewSignatureType(nil, nil, nil,
		types.NewTuple(types.NewVar(noPos, pkg, "ctx", ctxType)),
		types.NewTuple(
			types.NewVar(noPos, pkg, "", types.NewPointer(connection)),
			types.NewVar(noPos, pkg, "", errType),
		), false)))

	for _, name := range []string{
		"ErrInvalidArgument", "ErrNoConnection", "ErrBindingMismatch", "ErrClosed",
		"ErrRequestIDsExhausted", "ErrProtocol", "ErrInterfaceMismatch",
		"ErrProcedureNotFound", "ErrInvalidArguments", "ErrInternalException",
	} {
		insert(types.NewVar(noPos, pkg, name, errType))
	}

	pkg.MarkComplete()
	return pkg, nil
}
