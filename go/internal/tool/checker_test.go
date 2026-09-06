package tool

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// This file tests the generated-Go type checking of RM-13: the checker
// probes (duplicate declarations and parameters, undefined names,
// import/declaration collisions, wrong runtime SPI calls, and body-level
// type errors), the durable parity of the synthetic runtime SPI model
// against the actual root package, the exact discovery-load package
// identity preservation, and the production checking of depth-4,096
// import and export output under a bounded subprocess stack with the
// 4,097 preflight rejection before checker entry.

// TestGeneratedGoTypeChecking drives the production import checker over
// crafted binding sources: every required regression class is rejected
// at its exact position in the generated file, every diagnostic names
// only the logical binding path, and a valid generated binding passes.
func TestGeneratedGoTypeChecking(t *testing.T) {
	checker := NewImportGoChecker()
	const logical = "out/binding_gen.go"
	probes := []struct {
		name string
		src  string
		want string
	}{
		{
			"duplicate declarations",
			"package p\n\nvar x int\n\nvar x int\n",
			"redeclared",
		},
		{
			"duplicate parameters",
			"package p\n\nfunc f(a int, a int) {}\n",
			"redeclared",
		},
		{
			"undefined names",
			"package p\n\nvar x = missing\n",
			"undefined: missing",
		},
		{
			"import declaration collision",
			"package p\n\nimport \"context\"\n\nvar context = 1\n",
			"already declared",
		},
		{
			"wrong runtime SPI call",
			"package p\n\nimport ic \"github.com/cerasos/intercall/go\"\n\nvar b = ic.NewImportBinding(1)\n",
			"too many arguments",
		},
		{
			"wrong runtime SPI type use",
			"package p\n\nimport ic \"github.com/cerasos/intercall/go\"\n\nvar b int = ic.ErrProcedureNotFound\n",
			"cannot use",
		},
		{
			"body-level type errors",
			"package p\n\nfunc f() { var x int; x = \"s\" }\n",
			"cannot use",
		},
	}
	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			err := checker.CheckGo(logical, []byte(tc.src))
			if err == nil {
				t.Fatalf("the probe %q type-checked, want a rejection", tc.name)
			}
			var de *Error
			if !errors.As(err, &de) {
				t.Fatalf("error type %T, want *Error: %v", err, err)
			}
			if de.Filename != logical {
				t.Errorf("diagnostic filename = %q, want the logical binding path %q", de.Filename, logical)
			}
			if de.Pos.Line == 0 || de.Pos.Column == 0 {
				t.Errorf("diagnostic position = %+v, want a physical position in the generated file", de.Pos)
			}
			if !strings.Contains(de.Msg, tc.want) {
				t.Errorf("diagnostic message %q does not contain %q", de.Msg, tc.want)
			}
			if filepath.IsAbs(de.Filename) || strings.Contains(de.Msg, ".tmp-") {
				t.Errorf("diagnostic exposes an absolute or staging path: %v", de)
			}
		})
	}

	// A valid generated import binding passes the production checker.
	goFile, _, err := generateImportString("procedure ping {};\ntype t uint32;\nexception denied;\n", "imp")
	if err != nil {
		t.Fatalf("GenerateImport: %v", err)
	}
	if err := checker.CheckGo(logical, goFile); err != nil {
		t.Fatalf("the production checker rejected a valid generated binding: %v", err)
	}
}

// TestRuntimeSPIModelParity compares every exported object of the
// synthetic runtime SPI model with the actual root package, object by
// object: the exported object sets must be in bijection, the kinds must
// match, and the exported signatures, methods, and types must be
// structurally equal. The model and the actual package are built
// against one shared standard-library importer, so their context and io
// packages are literally the same instances; the runtime's own types
// are compared by exported surface, since generated code compiles
// against their names and signatures, never their unexported
// structure.
func TestRuntimeSPIModelParity(t *testing.T) {
	mi := &moduleImporter{fset: token.NewFileSet(), parsed: make(map[string]*types.Package)}
	actual, err := mi.Import(runtimeImportPath)
	if err != nil {
		t.Fatalf("loading the actual root package: %v", err)
	}
	model, err := buildRuntimeSPIModel(mi)
	if err != nil {
		t.Fatalf("building the synthetic runtime SPI model: %v", err)
	}
	if model.Path() != runtimeImportPath || model.Name() != "intercall" {
		t.Fatalf("model identity = %q/%q, want %q/intercall", model.Path(), model.Name(), runtimeImportPath)
	}

	// The model's standard library identities are the very packages the
	// actual root package was checked against.
	mctx := model.Scope().Lookup("ConnectionFromContext").(*types.Func).Type().(*types.Signature).Params().At(0).Type()
	actx := actual.Scope().Lookup("ConnectionFromContext").(*types.Func).Type().(*types.Signature).Params().At(0).Type()
	if mctx != actx {
		t.Fatal("the model's context.Context is not the package instance the actual root package used")
	}

	// Every exported object of the actual package is modeled, and every
	// modeled object exists in the actual package.
	for _, name := range actual.Scope().Names() {
		if !token.IsExported(name) {
			continue
		}
		mobj := model.Scope().Lookup(name)
		if mobj == nil {
			t.Errorf("the model lacks the exported root object %q", name)
			continue
		}
		if msg := parityObject(name, mobj, actual.Scope().Lookup(name)); msg != "" {
			t.Errorf("model object %q differs from the root package: %s", name, msg)
		}
	}
	for _, name := range model.Scope().Names() {
		if token.IsExported(name) && actual.Scope().Lookup(name) == nil {
			t.Errorf("the model has an exported object %q the root package does not export", name)
		}
	}

	// The method-set comparison is a durable drift regression: a model
	// whose Connection.Call carries an extra parameter must be reported
	// as differing from the root package, while the unaltered model
	// keeps matching. Without the method comparison, the modeled
	// Call/Close/Wait/WaitForHandlers signatures would not be pinned to the
	// root package.
	t.Run("modeled method drift is detected", func(t *testing.T) {
		altered := alteredConnection(t, model)
		if msg := parityObject("Connection", altered, actual.Scope().Lookup("Connection")); msg == "" {
			t.Fatal("an altered modeled Call signature was not detected by the parity comparison")
		}
		if msg := parityObject("Connection", model.Scope().Lookup("Connection"), actual.Scope().Lookup("Connection")); msg != "" {
			t.Fatalf("the unaltered model no longer matches the root package: %s", msg)
		}
	})
}

// TestRuntimeSPIMetadataCheckerProbes pins the generated-source checker
// boundary for the interface-aware runtime additions. Correct constructor
// and accessor uses type-check, while wrong ID types, missing arguments, and
// wrong result arity are rejected by the synthetic model.
func TestRuntimeSPIMetadataCheckerProbes(t *testing.T) {
	checker := NewImportGoChecker()
	const logical = "metadata/binding_gen.go"
	probes := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "correct constructors and accessors",
			src: `package p

import (
	"context"
	ic "github.com/cerasos/intercall/go"
)

var (
	id ic.InterfaceID
	export, exportErr = ic.NewExportBindingWithInterfaceID(func(context.Context, uint64, []byte) (uint64, []byte) { return 0, nil }, id)
	imp = ic.NewImportBindingWithInterfaceID(id)
	emptyExport = ic.EmptyExportBinding()
	emptyImport = ic.EmptyImportBinding()
	stream ic.ByteStream
	conn, connErr = ic.NewNegotiatedClientConnection(nil, stream, export, imp)
	serverConn, serverErr = ic.NewNegotiatedServerConnection(nil, stream, export, imp)
	errMismatch = ic.ErrInterfaceMismatch
	exportID, exportOK = export.InterfaceID()
	importID, importOK = imp.InterfaceID()
)
`,
		},
		{
			name: "wrong ID type",
			src: `package p

import ic "github.com/cerasos/intercall/go"

var imp = ic.NewImportBindingWithInterfaceID([16]byte{})
`,
			want: "cannot use",
		},
		{
			name: "missing ID argument",
			src: `package p

import ic "github.com/cerasos/intercall/go"

var imp = ic.NewImportBindingWithInterfaceID()
`,
			want: "not enough arguments",
		},
		{
			name: "wrong result arity",
			src: `package p

import ic "github.com/cerasos/intercall/go"

var one, two = ic.NewImportBindingWithInterfaceID(ic.InterfaceID{})
`,
			want: "assignment mismatch",
		},
	}
	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			err := checker.CheckGo(logical, []byte(tc.src))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checker rejected valid source: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checker accepted invalid source")
			}
			var de *Error
			if !errors.As(err, &de) {
				t.Fatalf("error type %T, want *Error: %v", err, err)
			}
			if !strings.Contains(de.Msg, tc.want) {
				t.Errorf("diagnostic %q does not contain %q", de.Msg, tc.want)
			}
		})
	}
}

// TestRuntimeSPIParityTargetedDrift pins the individual parity dimensions
// added for interface-aware bindings: InterfaceID's array length, one
// metadata-aware constructor signature, and one binding accessor method.
func TestRuntimeSPIParityTargetedDrift(t *testing.T) {
	mi := &moduleImporter{fset: token.NewFileSet(), parsed: make(map[string]*types.Package)}
	actual, err := mi.Import(runtimeImportPath)
	if err != nil {
		t.Fatalf("loading the actual root package: %v", err)
	}
	model, err := buildRuntimeSPIModel(mi)
	if err != nil {
		t.Fatalf("building the synthetic runtime SPI model: %v", err)
	}

	t.Run("InterfaceID array length", func(t *testing.T) {
		pkg := types.NewPackage(runtimeImportPath, "intercall")
		tn := types.NewTypeName(token.NoPos, pkg, "InterfaceID", nil)
		types.NewNamed(tn, types.NewArray(types.Typ[types.Byte], 31), nil)
		if msg := parityObject("InterfaceID", tn, actual.Scope().Lookup("InterfaceID")); msg == "" {
			t.Fatal("array-length drift was not detected")
		}
	})

	t.Run("metadata constructor signature", func(t *testing.T) {
		actualFn := actual.Scope().Lookup("NewImportBindingWithInterfaceID").(*types.Func)
		sig := actualFn.Type().(*types.Signature)
		altered := types.NewFunc(token.NoPos, types.NewPackage(runtimeImportPath, "intercall"), actualFn.Name(), types.NewSignatureType(nil, nil, nil, types.NewTuple(), sig.Results(), false))
		if msg := parityObject(actualFn.Name(), altered, actualFn); msg == "" {
			t.Fatal("constructor signature drift was not detected")
		}
	})

	t.Run("binding accessor method", func(t *testing.T) {
		pkg := types.NewPackage(runtimeImportPath, "intercall")
		tn := types.NewTypeName(token.NoPos, pkg, "ImportBinding", nil)
		named := types.NewNamed(tn, types.NewStruct(nil, nil), nil)
		methodSig := types.NewSignatureType(types.NewVar(token.NoPos, pkg, "b", named), nil, nil,
			types.NewTuple(), types.NewTuple(types.NewVar(token.NoPos, pkg, "", types.Typ[types.Bool])), false)
		named.AddMethod(types.NewFunc(token.NoPos, pkg, "InterfaceID", methodSig))
		if msg := parityObject("ImportBinding", tn, model.Scope().Lookup("ImportBinding")); msg == "" {
			t.Fatal("binding accessor method drift was not detected")
		}
	})
}

// alteredConnection builds one model-shaped Connection whose Call
// method carries an extra int parameter, with Close, Wait, and
// WaitForHandlers identical to the real model, so the parity comparison can
// only attribute the difference to the Call signature.
func alteredConnection(t *testing.T, model *types.Package) types.Object {
	t.Helper()
	noPos := token.NoPos
	pkg := types.NewPackage(runtimeImportPath, "intercall")
	tn := types.NewTypeName(noPos, pkg, "Connection", nil)
	conn := types.NewNamed(tn, types.NewStruct(nil, nil), nil)
	pkg.Scope().Insert(tn)

	ctxType := model.Scope().Lookup("ConnectionFromContext").(*types.Func).Type().(*types.Signature).Params().At(0).Type()
	importBinding := model.Scope().Lookup("ImportBinding").Type()
	requestEncoder := model.Scope().Lookup("RequestEncoder").Type()
	responseDecoder := model.Scope().Lookup("ResponseDecoder").Type()
	errType := types.Universe.Lookup("error").Type()

	addMethod := func(name string, params, results *types.Tuple) {
		sig := types.NewSignatureType(types.NewVar(noPos, pkg, "c", types.NewPointer(conn)), nil, nil, params, results, false)
		conn.AddMethod(types.NewFunc(noPos, pkg, name, sig))
	}
	addMethod("Call",
		types.NewTuple(
			types.NewVar(noPos, pkg, "ctx", ctxType),
			types.NewVar(noPos, pkg, "imp", importBinding),
			types.NewVar(noPos, pkg, "procedureKey", types.Typ[types.Uint64]),
			types.NewVar(noPos, pkg, "encode", requestEncoder),
			types.NewVar(noPos, pkg, "decode", responseDecoder),
			types.NewVar(noPos, pkg, "extra", types.Typ[types.Int]),
		),
		types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	addMethod("Close", types.NewTuple(), types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	addMethod("Wait", types.NewTuple(), types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	addMethod("WaitForHandlers", types.NewTuple(), types.NewTuple(types.NewVar(noPos, pkg, "", errType)))
	return tn
}

// parityObject compares one modeled object with the actual root package
// object of the same name: the kind must match, and the type or
// signature surface must be structurally equal. It returns "" when the
// objects are equal and an actionable message otherwise.
func parityObject(name string, m, a types.Object) string {
	switch mo := m.(type) {
	case *types.Func:
		ao, ok := a.(*types.Func)
		if !ok {
			return fmt.Sprintf("kind mismatch: the model declares a function, the root package a %T", a)
		}
		ms, as := mo.Type().(*types.Signature), ao.Type().(*types.Signature)
		if !bridgeSignatureEqual(ms, as) {
			return fmt.Sprintf("signature mismatch: model %s, root %s", ms, as)
		}
	case *types.Var:
		ao, ok := a.(*types.Var)
		if !ok {
			return fmt.Sprintf("kind mismatch: the model declares a variable, the root package a %T", a)
		}
		if !bridgeTypesEqual(mo.Type(), ao.Type()) {
			return fmt.Sprintf("type mismatch: model %s, root %s", mo.Type(), ao.Type())
		}
	case *types.TypeName:
		ao, ok := a.(*types.TypeName)
		if !ok {
			return fmt.Sprintf("kind mismatch: the model declares a type, the root package a %T", a)
		}
		if !bridgeTypesEqual(mo.Type(), ao.Type()) {
			return fmt.Sprintf("type mismatch: model %s, root %s", mo.Type(), ao.Type())
		}
	default:
		return fmt.Sprintf("unsupported modeled object kind %T", m)
	}
	return ""
}

// bridgeSignatureEqual compares two signatures structurally: variadic
// flag, parameter types, and result types.
func bridgeSignatureEqual(a, b *types.Signature) bool {
	return bridgeSignatureEqualSeen(a, b, make(map[[2]types.Type]bool))
}

// bridgeSignatureEqualSeen is bridgeSignatureEqual with a shared
// visited-pair set, so a self-referential named type reached through a
// signature (for example time.Time through context.Context.Deadline)
// terminates instead of recursing forever.
func bridgeSignatureEqualSeen(a, b *types.Signature, seen map[[2]types.Type]bool) bool {
	return a.Variadic() == b.Variadic() && bridgeTupleEqualSeen(a.Params(), b.Params(), seen) && bridgeTupleEqualSeen(a.Results(), b.Results(), seen)
}

// bridgeTupleEqualSeen compares two tuples structurally with a shared
// visited-pair set.
func bridgeTupleEqualSeen(a, b *types.Tuple, seen map[[2]types.Type]bool) bool {
	if a.Len() != b.Len() {
		return false
	}
	for i := 0; i < a.Len(); i++ {
		if !bridgeTypesEqualSeen(a.At(i).Type(), b.At(i).Type(), seen) {
			return false
		}
	}
	return true
}

// bridgeTypesEqual compares two types for the runtime-SPI parity test.
// Basic kinds must match; named types match by package path and name
// (the model's instances are necessarily distinct from the root
// package's), with their exported method sets compared by name and
// signature and their underlying types compared recursively; structs
// compare their exported fields by name and type; interfaces compare
// their exported methods; and arrays, slices, pointers, chans, maps,
// tuples, and signatures compare recursively. Unexported members are
// implementation details of the root package and are not part of the
// generated-code bridge surface.
func bridgeTypesEqual(a, b types.Type) bool {
	return bridgeTypesEqualSeen(a, b, make(map[[2]types.Type]bool))
}

// bridgeTypesEqualSeen is bridgeTypesEqual with a shared visited-pair
// set. The parity inputs are finite types, so a re-visit of a named
// pair currently being compared is a reference cycle: the pair is
// already being proven equal on the current path, which is exactly the
// coinductive equality of the two finite types.
func bridgeTypesEqualSeen(a, b types.Type, seen map[[2]types.Type]bool) bool {
	switch x := a.(type) {
	case *types.Basic:
		y, ok := b.(*types.Basic)
		return ok && x.Kind() == y.Kind()
	case *types.Array:
		y, ok := b.(*types.Array)
		return ok && x.Len() == y.Len() && bridgeTypesEqualSeen(x.Elem(), y.Elem(), seen)
	case *types.Named:
		y, ok := b.(*types.Named)
		if !ok || x.Obj().Name() != y.Obj().Name() || !sameBridgePkg(x.Obj().Pkg(), y.Obj().Pkg()) {
			return false
		}
		key := [2]types.Type{x, y}
		if seen[key] {
			return true
		}
		seen[key] = true
		// The exported method sets are part of the generated-code
		// bridge surface: Connection's Call, Close, Wait, and
		// WaitForHandlers are modeled signatures, so a method drift in either
		// direction must fail the parity test.
		if !bridgeMethodsEqual(x, y, seen) {
			return false
		}
		return bridgeTypesEqualSeen(x.Underlying(), y.Underlying(), seen)
	case *types.Alias:
		y, ok := b.(*types.Alias)
		return ok && bridgeTypesEqualSeen(types.Unalias(x), types.Unalias(y), seen)
	case *types.Pointer:
		y, ok := b.(*types.Pointer)
		return ok && bridgeTypesEqualSeen(x.Elem(), y.Elem(), seen)
	case *types.Slice:
		y, ok := b.(*types.Slice)
		return ok && bridgeTypesEqualSeen(x.Elem(), y.Elem(), seen)
	case *types.Chan:
		y, ok := b.(*types.Chan)
		return ok && x.Dir() == y.Dir() && bridgeTypesEqualSeen(x.Elem(), y.Elem(), seen)
	case *types.Map:
		y, ok := b.(*types.Map)
		return ok && bridgeTypesEqualSeen(x.Key(), y.Key(), seen) && bridgeTypesEqualSeen(x.Elem(), y.Elem(), seen)
	case *types.Struct:
		y, ok := b.(*types.Struct)
		if !ok {
			return false
		}
		var xf, yf []*types.Var
		for i := 0; i < x.NumFields(); i++ {
			if token.IsExported(x.Field(i).Name()) {
				xf = append(xf, x.Field(i))
			}
		}
		for i := 0; i < y.NumFields(); i++ {
			if token.IsExported(y.Field(i).Name()) {
				yf = append(yf, y.Field(i))
			}
		}
		if len(xf) != len(yf) {
			return false
		}
		for i := range xf {
			if xf[i].Name() != yf[i].Name() || !bridgeTypesEqualSeen(xf[i].Type(), yf[i].Type(), seen) {
				return false
			}
		}
		return true
	case *types.Interface:
		y, ok := b.(*types.Interface)
		if !ok || x.NumMethods() != y.NumMethods() {
			return false
		}
		for i := 0; i < x.NumMethods(); i++ {
			xm, ym := x.Method(i), y.Method(i)
			if xm.Name() != ym.Name() || !bridgeSignatureEqualSeen(xm.Type().(*types.Signature), ym.Type().(*types.Signature), seen) {
				return false
			}
		}
		return true
	case *types.Signature:
		y, ok := b.(*types.Signature)
		return ok && bridgeSignatureEqualSeen(x, y, seen)
	case *types.Tuple:
		y, ok := b.(*types.Tuple)
		return ok && bridgeTupleEqualSeen(x, y, seen)
	}
	return false
}

// bridgeMethodsEqual compares the exported method sets of two named
// types: a bijection by name whose signatures compare structurally.
// Unexported methods are implementation details of the root package
// and are not part of the generated-code bridge surface.
func bridgeMethodsEqual(a, b *types.Named, seen map[[2]types.Type]bool) bool {
	var am []*types.Func
	for i := 0; i < a.NumMethods(); i++ {
		if m := a.Method(i); token.IsExported(m.Name()) {
			am = append(am, m)
		}
	}
	bm := make(map[string]*types.Func, b.NumMethods())
	for i := 0; i < b.NumMethods(); i++ {
		if m := b.Method(i); token.IsExported(m.Name()) {
			bm[m.Name()] = m
		}
	}
	if len(am) != len(bm) {
		return false
	}
	for _, m := range am {
		o, ok := bm[m.Name()]
		if !ok || !bridgeSignatureEqualSeen(m.Type().(*types.Signature), o.Type().(*types.Signature), seen) {
			return false
		}
	}
	return true
}

// sameBridgePkg reports whether two package references are the same
// package: both nil, or the same import path.
func sameBridgePkg(a, b *types.Package) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Path() == b.Path()
}

// TestGeneratedCheckerLoadedTypeIdentity proves that export checking
// reuses the exact *types.Package identities of the one combined
// discovery load: a loaded provider whose signature uses
// context.Context and a reachable cross-package named type passes the
// complete pipeline only because the generated binding's imports
// resolve to the very package instances the provider was mapped
// against. The white-box assertions pin the identity table directly,
// and the staged write proves the type check succeeds on those
// identities.
func TestGeneratedCheckerLoadedTypeIdentity(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/idn\n\ngo 1.26.5\n",
		"types/types.go": `// Package types declares the reachable cross-package named type.
package types

// Token is the reachable named type of the identity fixture.
// @intercall type token
type Token struct {
	V uint32
}
`,
		"prov/prov.go": `// Package prov is the identity fixture provider.
package prov

import (
	"context"

	"example.com/idn/types"
)

// Find resolves one token.
// @intercall procedure find
func Find(ctx context.Context, t types.Token) (types.Token, error) { return t, nil }
`,
	})
	res, err := discover(t, dir, []string{"./prov"}, nil, nil, "./out")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	model, err := MapExport(res, res.OutPath)
	if err != nil {
		t.Fatalf("MapExport: %v", err)
	}
	checker := NewExportGoChecker(res)
	ec := checker.(*exportGoChecker)

	// The checker's identity table holds the very *types.Package
	// instances of the one combined discovery load.
	p := res.Providers[0]
	ctxType := p.Pkg.pkg.TypesInfo.TypeOf(p.Func.Type.Params.List[0].Type)
	if ctxType == nil {
		t.Fatal("no type information for the provider's context parameter")
	}
	loadCtx := ctxType.(*types.Named).Obj().Pkg()
	if got := ec.imp.graph["context"]; got != loadCtx {
		t.Fatal("the checker resolved context to a different package instance than the discovery load")
	}
	tokType := p.Pkg.pkg.TypesInfo.TypeOf(p.Func.Type.Params.List[1].Type)
	if tokType == nil {
		t.Fatal("no type information for the provider's named-type parameter")
	}
	loadTypes := tokType.(*types.Named).Obj().Pkg()
	if got := ec.imp.graph["example.com/idn/types"]; got != loadTypes {
		t.Fatal("the checker resolved the named-type package to a different instance than the discovery load")
	}

	// The complete pipeline with the production checker succeeds only
	// when the generated binding's imports resolve to those exact
	// identities: a context.Context of any other package instance fails
	// the provider call, and a different named-type package instance
	// fails the parameter assignment.
	goFile, body, err := GenerateExport(model, "gen")
	if err != nil {
		t.Fatalf("GenerateExport: %v", err)
	}
	out := t.TempDir()
	if err := WriteArtifacts(WriteConfig{
		Mode:          ExportMode,
		OutDir:        out,
		Package:       "gen",
		InterfacePath: filepath.Join(out, "api.intercall"),
		GoFile:        goFile,
		InterfaceBody: body,
		CheckGo:       checker,
	}); err != nil {
		t.Fatalf("the generated binding does not type-check against the discovery identities: %v", err)
	}
}

// importCheckerHybridSource renders the structural list/record chain of
// k declarations with a procedure taking and returning the chain head,
// so the complete generated binding exercises the caller and the
// runtime SPI at the boundary. The declaration walk's deepest
// occurrence (the head record's field) sits at depth 2k; the procedure
// roots re-walk the chain two levels deeper because the head's
// underlying was never walked at a deeper root depth, so the boundary
// shape uses k = maxProjectionDepth/2-1 with the deepest occurrence at
// exactly 4,096, and k = maxProjectionDepth/2 is rejected.
func importCheckerHybridSource(k int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type t0 record { f uint8; };\n")
	for i := 1; i < k; i++ {
		fmt.Fprintf(&b, "type t%d list t%d;\n", i, i-1)
	}
	fmt.Fprintf(&b, "procedure p { x t%d; } t%d;\n", k-1, k-1)
	return b.String()
}

// importCheckerNamedSource renders the named chain of k declarations
// with a procedure taking and returning a mid-chain type. The deepest
// occurrence (the final primitive through the head declaration's walk)
// sits at depth k, and the procedure roots re-walk nothing because the
// mid-chain type's underlying was already walked at a deeper root
// depth.
//
// The pure defined-type chain is not exercised at the 4,096 boundary:
// go/types resolves a defined-type chain's underlyings eagerly, so
// checking the generated binding of a 4,096-declaration pure named
// chain is cubic in the declaration count and does not terminate in a
// bounded test budget. The slice-interleaved chains of
// importCheckerHybridSource are the native-representability boundary
// shape for import checking; every declaration of that chain is a
// named type, so the checker is exercised over 2,047 chained named
// declarations at the boundary.
const importCheckerBoundaryEnv = "INTERCALL_RM13_IMPORT_CHECKER_BOUNDARY"

// TestGeneratedImportCheckerProjectionBoundary proves that actual
// production checking accepts depth-4,096 import output under a bounded
// subprocess stack: the structural list/record chain and the named
// chain generate completely and type-check through the artifact write,
// byte-deterministically, and depth-4,097 shapes are rejected by the
// preflight before checker entry.
func TestGeneratedImportCheckerProjectionBoundary(t *testing.T) {
	if os.Getenv(importCheckerBoundaryEnv) == "1" {
		runImportCheckerBoundarySubprocess(t)
		return
	}
	deepSubprocess(t, importCheckerBoundaryEnv, "TestGeneratedImportCheckerProjectionBoundary")
}

// stageImportBinding stages one generated import binding through the
// complete artifact pipeline with the production checker.
func stageImportBinding(t *testing.T, checker GoChecker, goFile, body []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := WriteArtifacts(WriteConfig{
		Mode:          ImportMode,
		OutDir:        dir,
		Package:       "imp",
		GoFile:        goFile,
		InterfaceBody: body,
		CheckGo:       checker,
	}); err != nil {
		t.Fatalf("production checking rejected the depth-4096 import binding: %v", err)
	}
}

func runImportCheckerBoundarySubprocess(t *testing.T) {
	debug.SetMaxStack(deepMaxStackBytes)
	checker := NewImportGoChecker()

	// The structural list/record chain at exactly 4,096: 2,047
	// declarations put the head record's field at depth 4,096 including
	// the procedure roots. The complete generation — model, codec
	// pairs, caller, and semantic constant — runs under the lowered
	// stack, the production checker accepts the output, and repeated
	// generation is byte-identical.
	hybrid := importCheckerHybridSource(maxProjectionDepth/2 - 1)
	goFile, body := deepMustGenerate(t, hybrid)
	stageImportBinding(t, checker, goFile, body)
	goFile2, body2 := deepMustGenerate(t, hybrid)
	if !bytes.Equal(goFile, goFile2) || !bytes.Equal(body, body2) {
		t.Fatal("boundary import generation is not deterministic")
	}

	// Depth 4,097 is rejected by the preflight before checker entry:
	// generation fails with the stable depth diagnostic and no binding
	// bytes exist to check, for the pure structural shape and for the
	// interleaved chain one declaration deeper.
	_, _, err := GenerateImport("deep.intercall", []byte(importListSource(maxProjectionDepth)), nil, "imp")
	depthErrorOf(t, err)
	_, _, err = GenerateImport("deep.intercall", []byte(importCheckerHybridSource(maxProjectionDepth/2)), nil, "imp")
	depthErrorOf(t, err)
}

const exportCheckerBoundaryEnv = "INTERCALL_RM13_EXPORT_CHECKER_BOUNDARY"

// TestGeneratedExportCheckerProjectionBoundary proves that actual
// production checking accepts depth-4,096 export output under a bounded
// subprocess stack: the alternating struct/slice chain (structural),
// the defined-type chain (named), the alias chain, and the
// trusted-metadata cross-row chain all generate completely and
// type-check through the artifact write, byte-deterministically, and
// depth-4,097 shapes are rejected by the preflight before checker
// entry.
//
// The structural shape at the boundary is the alternating struct/slice
// chain, not a parameter of 4,095 nested slices: the pure slice
// parameter's one shared codec pair nests 4,095 loop levels, whose
// emitted indentation is quadratic in the depth, so the generated file
// of the pure shape is impractically large; the alternating chain is
// the same structural depth with linear emission, exactly as on the
// import side.
func TestGeneratedExportCheckerProjectionBoundary(t *testing.T) {
	if os.Getenv(exportCheckerBoundaryEnv) == "1" {
		runExportCheckerBoundarySubprocess(t)
		return
	}
	deepSubprocess(t, exportCheckerBoundaryEnv, "TestGeneratedExportCheckerProjectionBoundary")
}

// exportBoundaryModule renders one synthetic module whose ./synth
// package is pkgSrc.
func exportBoundaryModule(pkgSrc string) map[string]string {
	return map[string]string{
		"go.mod":         "module example.com/synth\n\ngo 1.26.5\n",
		"synth/synth.go": pkgSrc,
	}
}

// stageExportChain runs the complete export pipeline over one synthetic
// module with the production checker: real discovery, the export model,
// the emitter, and the artifact write, plus a deterministic second
// emission.
func stageExportChain(t *testing.T, files map[string]string) {
	t.Helper()
	dir := writeFixture(t, files)
	res, err := discover(t, dir, []string{"./synth"}, nil, nil, "./out")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	model, err := MapExport(res, res.OutPath)
	if err != nil {
		t.Fatalf("MapExport: %v", err)
	}
	checker := NewExportGoChecker(res)
	goFile, body, err := GenerateExport(model, "gen")
	if err != nil {
		t.Fatalf("GenerateExport: %v", err)
	}
	out := t.TempDir()
	if err := WriteArtifacts(WriteConfig{
		Mode:          ExportMode,
		OutDir:        out,
		Package:       "gen",
		InterfacePath: filepath.Join(out, "api.intercall"),
		GoFile:        goFile,
		InterfaceBody: body,
		CheckGo:       checker,
	}); err != nil {
		t.Fatalf("production checking rejected the depth-4096 export binding: %v", err)
	}
	goFile2, body2, err := GenerateExport(model, "gen")
	if err != nil {
		t.Fatalf("GenerateExport (second pass): %v", err)
	}
	if !bytes.Equal(goFile, goFile2) || !bytes.Equal(body, body2) {
		t.Fatal("boundary export generation is not deterministic")
	}
}

func runExportCheckerBoundarySubprocess(t *testing.T) {
	debug.SetMaxStack(deepMaxStackBytes)

	// The structural chain at exactly 4,096: 2,048 alternating
	// one-field struct and slice declarations put the deepest
	// occurrence at depth 4,096, and every generated codec pair nests
	// exactly one structural level.
	stageExportChain(t, exportBoundaryModule(exportHybridPackage(maxProjectionDepth/2)))

	// The named defined-type chain at exactly 4,096.
	stageExportChain(t, exportBoundaryModule(exportChainPackage(maxProjectionDepth/2)))

	// The alias chain at exactly 4,096.
	stageExportChain(t, exportBoundaryModule(exportAliasPackage(maxProjectionDepth-1)))

	// The trusted-metadata cross-row chain at exactly 4,096: 2,048 rows
	// of a marked generated file with the provider taking the head row.
	files := metadataModuleFiles(t, maxProjectionDepth/2)
	rebased := make(map[string]string, len(files)+1)
	for name, content := range files {
		rebased["synth/"+name] = content
	}
	rebased["go.mod"] = "module example.com/synth\n\ngo 1.26.5\n"
	stageExportChain(t, rebased)

	// Depth 4,097 is rejected by the preflight before checker entry for
	// every shape: the pure slice parameter chain, the structural,
	// named, and alias chains, and the reached trusted-metadata chain.
	exportErr(t, "example.com/synth", exportSlicePackage(maxProjectionDepth),
		"exceeds the strict Go projection depth limit of 4096 occurrences")
	exportErr(t, "example.com/synth", exportHybridPackage(maxProjectionDepth/2+1),
		"exceeds the strict Go projection depth limit of 4096 occurrences")
	exportErr(t, "example.com/synth", exportChainPackage(maxProjectionDepth/2+1),
		"exceeds the strict Go projection depth limit of 4096 occurrences")
	exportErr(t, "example.com/synth", exportAliasPackage(maxProjectionDepth),
		"exceeds the strict Go projection depth limit of 4096 occurrences")
	_, err := metadataModel(t, metadataModuleFiles(t, maxProjectionDepth/2+1))
	depthErrorOf(t, err)
}
