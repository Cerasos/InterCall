package tool

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"
)

// This file tests the export emitter: the static procedure key switch,
// the direct application-exception matcher, the request decoders, the
// fixed exception selection, the deterministic imports, the immutable
// export binding singleton, and deterministic byte output. The
// checked-in fixture is the byte-exact generation golden; these tests
// additionally check generated shapes over the fixture and synthetic
// models.

// aliasProvA and aliasProvB are two same-named provider packages of
// the import-alias collision test.
const (
	aliasProvA = `// Package prov is the first provider package.
package prov

import "context"

// @intercall procedure a
func A(ctx context.Context) error { return nil }
`
	aliasProvB = `// Package prov is the second provider package.
package prov

import "context"

// @intercall procedure b
func B(ctx context.Context) error { return nil }
`
)

// TestExportGeneration checks the generated shapes of the fixture and
// of synthetic models: the exact switch cases and fixed keys, the
// direct equality and assertion matcher arms with the typed-nil guard
// and the exactly-one-match fallback, the provider imports and
// wrapper calls with the handler context first, the request decoders,
// the binding singleton, and the byte-exact golden interface.
func TestExportGeneration(t *testing.T) {
	t.Run("fixture shapes", func(t *testing.T) {
		goFile, body, _ := generateExportFixture(t)
		gen := string(goFile)
		// The deterministic provider import and the fixed standard
		// library imports.
		for _, want := range []string{
			`"context"`,
			`"errors"`,
			`"github.com/cerasos/intercall/go"`,
			`"github.com/cerasos/intercall/go/internal/tool/exportfixture/prov"`,
			`"math"`,
			`"unicode/utf8"`,
		} {
			if !strings.Contains(gen, want) {
				t.Errorf("generated binding lacks the import %s", want)
			}
		}
		// The static switch dispatches on the procedure key with the
		// exact key literals and calls the providers with the handler
		// context first.
		for _, want := range []string{
			"func " + dispatchName + "(",
			"switch " + dispatchKeyName + " {",
			"case 0x159eb91a98f8f42:",  // echo
			"case 0x52095929e015a29f:", // paint
			"case 0x2549b9c663851f:",   // sample
			"prov.Echo(" + dispatchCtxName + ", v0)",
			"prov.Paint(" + dispatchCtxName + ", v0, v1)",
			"err = prov.Ping(" + dispatchCtxName + ")",
		} {
			if !strings.Contains(gen, want) {
				t.Errorf("generated binding lacks %q", want)
			}
		}
		// The fixed exception selection: unknown keys, malformed or
		// trailing arguments, and the fallback conditions.
		for _, want := range []string{
			"return 0x970e76fcc5e2dacb, nil", // procedure_not_found
			"return 0x3f5fc972f8477b07, nil", // invalid_arguments
			"return 0x1aaec22e85996f50, nil", // internal_exception
			"if err != nil || len(rest) != 0 {",
		} {
			if !strings.Contains(gen, want) {
				t.Errorf("generated binding lacks %q", want)
			}
		}
		// The direct matcher: equality for sentinels, assertion with
		// the typed-nil guard for payload structs, and the exact
		// one-match fallback. Fixed runtime exceptions never appear in
		// the matcher.
		for _, want := range []string{
			"func " + matcherName + "(err error) (uint64, []byte) {",
			"if err == error(prov.ErrDenied) {",
			"if e, ok := err.(*prov.Failed); ok && e != nil {",
			"if e, ok := err.(*prov.Shared); ok && e != nil {",
			"if match != 1 || encErr != nil {",
		} {
			if !strings.Contains(gen, want) {
				t.Errorf("generated binding lacks %q", want)
			}
		}
		for _, gone := range []string{
			"errors.Is", "errors.As", "err.(*prov.ErrDenied)",
		} {
			if strings.Contains(gen, gone) {
				t.Errorf("generated binding contains %q", gone)
			}
		}
		// The request decoders decode the provider parameter types.
		for _, want := range []string{
			"func " + codecName("dereq", "paint") + "(src []byte) (struct {",
			"func " + codecName("dereq", "fetch") + "(src []byte) (prov.UserID, []byte, error) {",
			"func " + codecName("dereq", "ping") + "(src []byte) ([]byte, error) {",
		} {
			if !strings.Contains(gen, want) {
				t.Errorf("generated binding lacks %q", want)
			}
		}
		// The immutable export binding singleton constructs the handle
		// once through the metadata-aware constructor.
		for _, want := range []string{
			"intercall.NewExportBindingWithInterfaceID(" + dispatchName + ",",
			"func ExportBinding() intercall.ExportBinding {",
		} {
			if !strings.Contains(gen, want) {
				t.Errorf("generated binding lacks %q", want)
			}
		}
		// The generated file carries no ownership marker itself; the
		// artifact layer composes it.
		if strings.Contains(gen, intercallGeneratedMarker) {
			t.Error("the generated binding Go file must not carry the ownership marker itself")
		}
		// The canonical interface body is exactly the golden body.
		golden, err := os.ReadFile(exportFixtureInterfaceGolden)
		if err != nil {
			t.Fatalf("reading the golden interface: %v", err)
		}
		wantBody := composeExportInterface(body)
		if !bytes.Equal(golden, wantBody) {
			t.Error("the canonical interface body does not compose to the golden interface")
		}
	})

	t.Run("empty interface", func(t *testing.T) {
		// Export inserts the three fixed runtime exceptions into every
		// interface; the dispatch of an empty interface has only the
		// default arm.
		model, err := MapExport(&DiscoverResult{}, "")
		if err != nil {
			t.Fatalf("MapExport: %v", err)
		}
		goFile, body, err := GenerateExport(model, "exp")
		if err != nil {
			t.Fatalf("GenerateExport(empty): %v", err)
		}
		want := "exception internal_exception;\n\nexception invalid_arguments;\n\nexception procedure_not_found;\n"
		if string(body) != want {
			t.Errorf("empty interface body = %q, want %q", body, want)
		}
		gen := string(goFile)
		if !strings.Contains(gen, "default:\n\t\treturn 0x970e76fcc5e2dacb, nil") {
			t.Error("the empty dispatch lacks the procedure_not_found default arm")
		}
		if !strings.Contains(gen, matcherName) {
			t.Error("an interface without application exceptions must still emit the fixed-error matcher")
		}
		if !strings.Contains(gen, "func ExportBinding() intercall.ExportBinding {") {
			t.Error("the empty binding lacks the ExportBinding function")
		}
		// The empty binding type-checks with no provider imports.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "binding_gen.go", goFile, parser.AllErrors)
		if err != nil {
			t.Fatalf("empty binding does not parse: %v", err)
		}
		mi := &moduleImporter{fset: fset, parsed: make(map[string]*types.Package)}
		if _, err := (&types.Config{Importer: mi}).Check("exp", fset, []*ast.File{f}, nil); err != nil {
			t.Fatalf("empty binding does not type-check: %v", err)
		}
	})

	t.Run("no application exceptions", func(t *testing.T) {
		// A provider-only interface still emits the fixed-sentinel matcher so
		// providers can return procedure_not_found and invalid_arguments.
		model := exportOne(t, "example.com/synth", `package synth

import "context"

// @intercall procedure ping
func Ping(ctx context.Context) error { return nil }
`)
		goFile, _, err := GenerateExport(model, "exp")
		if err != nil {
			t.Fatalf("GenerateExport: %v", err)
		}
		gen := string(goFile)
		if !strings.Contains(gen, matcherName) {
			t.Error("a provider-only interface must emit the fixed-error matcher")
		}
		if !strings.Contains(gen, "if err != nil {") || !strings.Contains(gen, "return 0x1aaec22e85996f50, nil") {
			t.Error("the provider-only dispatch lacks the direct internal_exception fallback")
		}
		if !strings.Contains(gen, "return "+matcherName) {
			t.Error("the provider-only dispatch must call the matcher")
		}
		typeCheckSyntheticExportBinding(t, map[string]string{"example.com/synth": `package synth

import "context"

// @intercall procedure ping
func Ping(ctx context.Context) error { return nil }
`}, goFile)
	})

	t.Run("import alias collisions", func(t *testing.T) {
		// Two explicit provider packages with the same package name get
		// distinct deterministic import aliases: the first in canonical
		// path order keeps the package name, the second is privately
		// mangled.
		dir := writeFixture(t, map[string]string{
			"go.mod":     "module example.com/aliasmod\n\ngo 1.26.5\n",
			"a/a.go":     aliasProvA,
			"b/b.go":     aliasProvB,
			"out/out.go": "// Package out is the output target.\npackage out\n",
		})
		res, err := discover(t, dir, []string{"./a", "./b"}, nil, nil, "out")
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		model, err := MapExport(res, "example.com/aliasmod/out")
		if err != nil {
			t.Fatalf("MapExport: %v", err)
		}
		goFile, _, err := GenerateExport(model, "out")
		if err != nil {
			t.Fatalf("GenerateExport: %v", err)
		}
		gen := string(goFile)
		mangled := ManglePrivate("import", "example.com/aliasmod/b")
		if !strings.Contains(gen, `"example.com/aliasmod/a"`) || !strings.Contains(gen, mangled+` "example.com/aliasmod/b"`) {
			t.Errorf("the alias collision was not resolved: a keeps the plain name, b must be %s", mangled)
		}
		if !strings.Contains(gen, "prov.A(") || !strings.Contains(gen, mangled+".B(") {
			t.Error("the providers must be called through their distinct aliases")
		}
		typeCheckSyntheticExportBinding(t, map[string]string{
			"example.com/aliasmod/a": aliasProvA,
			"example.com/aliasmod/b": aliasProvB,
		}, goFile)
	})

	t.Run("fixed import name collisions", func(t *testing.T) {
		// A provider package whose package name matches one of the
		// fixed imports gets a deterministically mangled alias; the
		// plain name stays with the fixed import, so the generated
		// binding compiles.
		for _, name := range []string{"context", "errors", "intercall", "math", "utf8"} {
			t.Run(name, func(t *testing.T) {
				src := "package " + name + `

import "context"

// @intercall procedure ping
func Ping(ctx context.Context) error { return nil }
`
				model := exportOne(t, "example.com/synth", src)
				goFile, _, err := GenerateExport(model, "exp")
				if err != nil {
					t.Fatalf("GenerateExport: %v", err)
				}
				gen := string(goFile)
				mangled := ManglePrivate("import", "example.com/synth")
				if !strings.Contains(gen, mangled+` "example.com/synth"`) {
					t.Errorf("provider package named %q keeps the fixed import name; want alias %s", name, mangled)
				}
				if !strings.Contains(gen, mangled+".Ping(") {
					t.Errorf("the provider must be called through its mangled alias %s", mangled)
				}
				typeCheckSyntheticExportBinding(t, map[string]string{"example.com/synth": src}, goFile)
			})
		}
	})

	t.Run("hostile parameter names", func(t *testing.T) {
		// Provider parameter names that collide with plain helper
		// names must not leak into the dispatch or the request
		// decoder: the decoded values are fresh case locals, and the
		// decoder locals are v0..vn.
		model := exportOne(t, "example.com/synth", `package synth

import "context"

// @intercall procedure p
func P(ctx context.Context, err string, ctxv uint8, out int64, rest string, v0 uint32) (string, error) {
	return "", nil
}
`)
		goFile, _, err := GenerateExport(model, "exp")
		if err != nil {
			t.Fatalf("GenerateExport: %v", err)
		}
		typeCheckSyntheticExportBinding(t, map[string]string{"example.com/synth": `package synth

import "context"

// @intercall procedure p
func P(ctx context.Context, err string, ctxv uint8, out int64, rest string, v0 uint32) (string, error) {
	return "", nil
}
`}, goFile)
	})
}

// TestExportGenerationDeterminism verifies that the export emitter is
// deterministic: the same discovery pass generates byte-identical
// output on repeated runs, and a different interface generates
// different bytes. The fixture comparison in
// TestExportGeneratedFixtureCompiles already pins the emitted bytes.
func TestExportGenerationDeterminism(t *testing.T) {
	first, firstBody, _ := generateExportFixture(t)
	second, secondBody, _ := generateExportFixture(t)
	if !bytes.Equal(first, second) || !bytes.Equal(firstBody, secondBody) {
		t.Fatal("generating the fixture twice produced different bytes")
	}

	// A second complete pass over a synthetic provider-only model is
	// likewise deterministic, and its bytes differ from the fixture.
	model := exportOne(t, "example.com/synth", `package synth

import "context"

// @intercall procedure ping
func Ping(ctx context.Context) error { return nil }
`)
	a, aBody, err := GenerateExport(model, "exp")
	if err != nil {
		t.Fatalf("GenerateExport: %v", err)
	}
	b, bBody, err := GenerateExport(model, "exp")
	if err != nil {
		t.Fatalf("GenerateExport (second run): %v", err)
	}
	if !bytes.Equal(a, b) || !bytes.Equal(aBody, bBody) {
		t.Fatal("generating the same synthetic model twice produced different bytes")
	}
	if bytes.Equal(a, first) {
		t.Fatal("a different interface produced the same binding bytes")
	}
}
