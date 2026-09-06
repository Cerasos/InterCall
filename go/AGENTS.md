# AGENTS.md

## Scope and sources of truth

These instructions apply to the Go module in this directory and all of its
subdirectories. Run Go commands from this directory.

Read the relevant design documents before changing behavior:

1. [`../README.md`](../README.md) is normative for the InterCall interface
   language and wire protocol.
2. [`SPEC.md`](SPEC.md) defines the current Go mapping, generator, CLI, and
   runtime. If it conflicts with `../README.md`, the README wins.
3. [`GO.md`](GO.md) is the user-facing build and usage guide.
4. [`PLAN.md`](PLAN.md) is a plan for future native Unix-socket and WebSocket
   work. It does not describe the currently implemented API. If explicitly
   implementing that plan, preserve its ordered task boundaries and update the
   normative documents as behavior is introduced.

Do not infer current behavior from `PLAN.md` when it disagrees with the code or
`SPEC.md`.

## Repository map

- The module is `github.com/cerasos/intercall/go` and requires the exact Go
  version declared in `go.mod`.
- Root package `intercall`: transport-independent runtime and generated-code
  SPI. It intentionally uses only the standard library.
- `cmd/intercall-go`: `export` and `import` CLI commands.
- `internal/syntax`: parser, validator, documentation attachment, formatter,
  keys, and exact source positions for `.intercall` files. It also uses only
  the standard library.
- `internal/tool`: Go discovery, mapping, codecs, generation, diagnostics,
  generated-source checking, and artifact ownership. `go/packages` is the only
  current third-party dependency.
- `internal/integration`: compiled generated bindings exercised as black-box
  peers over in-memory byte streams.
- `internal/**/testdata` and `testdata/fuzz`: golden files, malformed inputs,
  and fuzz corpora.
- `../intercall-validate.lua`: optional non-normative syntax-validation oracle.

## Core engineering constraints

### Protocol and runtime

- Do not change interface grammar, keys, value encoding, frame layout, or wire
  semantics without first reconciling `../README.md` and `SPEC.md`.
- The current raw runtime starts at the first InterCall frame. It does not dial,
  listen, authenticate, negotiate, or exchange interface metadata.
- Preserve the 24-byte little-endian frame header and the exact 64 MiB maximum
  accepted frame payload. Validate wire lengths before conversion, allocation,
  or payload reads.
- Keep the receive loop as the sole frame reader. Requests and responses share
  one write gate; frames must never interleave.
- Never hold the connection mutex while waiting for the write gate or calling a
  stream's `Read`, `Write`, or `Close`. Preserve the documented lock order.
- Preserve connection ownership and lifecycle semantics: first terminal cause
  wins, `Close` remains prompt, and `Wait` observes completed teardown without
  waiting for handlers that ignore cancellation. The additive owner-side
  `WaitForHandlers` barrier first joins that existing `Wait` completion and then
  waits for every admitted handler, including deferred same-ID generations;
  it returns the same terminal cause and must not be called by an active
  handler or by stream cleanup.
- Preserve exact context errors where specified. Wrapped transport and protocol
  errors must retain `errors.Is`/`errors.As` identity.
- Public error sentinels are comparable contracts. Do not casually change their
  identity or text.
- Any exported root API change must update the synthetic runtime model in
  `internal/tool/checker.go` and its durable parity tests in
  `internal/tool/checker_test.go` in the same change.

### Syntax and projection

- Parsing and syntax-owned walks must keep Go call-stack use independent of the
  interface grammar's unrestricted nesting. Use explicit stacks for unbounded
  type walks.
- Source positions are physical, one-based byte positions. Do not let `//line`
  directives or Unicode rune counting rewrite diagnostics.
- The protocol grammar itself has no nesting limit. The separate strict Go
  projection limit is exactly 4,096 resolved type occurrences.
- Maintain canonical formatting and semantic documentation attachment exactly;
  golden and metadata tests depend on byte-for-byte output.

### Generator and filesystem behavior

- Generated output must be deterministic: no timestamps, absolute paths,
  temporary paths, map-order dependence, or environment-dependent ordering.
- Complete source validation, projection, generated-content validation, and
  generated-Go type checking before filesystem mutation.
- Preserve artifact ownership rules. Never overwrite handwritten files, follow
  a target-leaf symlink, truncate an owned target in place, or delete stale
  paths. Owned replacements use staging and rename.
- Diagnostics use `path:line:column: message`, logical paths, physical byte
  positions, and deterministic sorting.
- Keep generated bindings static and thin. Do not add reflection, runtime
  registration, a handler registry, or imports of generator internals.
- Do not add dependencies to the root runtime or `internal/syntax`. Keep any
  approved transport-specific dependency isolated to its transport package.

## Generated files and fixtures

Files carrying `Code generated ... DO NOT EDIT` and owned `.intercall` files are
checked-in products, not source files. Important fixtures include:

- `internal/tool/fixture/codec_gen.go`
- `internal/tool/importfixture/binding_gen.go`
- `internal/tool/exportfixture/binding_gen.go`
- `internal/tool/testdata/export/export.intercall`
- `internal/integration/fixtures/e2eimport/binding_gen.go`
- `internal/integration/fixtures/e2eexport/binding_gen.go`
- `internal/integration/fixtures/interface/e2e.intercall`

Do not hand-edit these files. The fixture tests are read-only validators: they
stage fresh output in temporary directories and compare it byte for byte, but
they do not update checked-in files.

Regenerate the integration fixtures with the public CLI:

```sh
go run ./cmd/intercall-go export \
  --out internal/integration/fixtures/e2eexport \
  --interface internal/integration/fixtures/interface/e2e.intercall \
  ./internal/integration/fixtures/provider
go run ./cmd/intercall-go import \
  --out internal/integration/fixtures/e2eimport \
  internal/integration/fixtures/interface/e2e.intercall
```

The tool fixtures share directories with handwritten `surface.go` files, so
generate them in a temporary module directory and copy only the generated
products:

```sh
tmp=$(mktemp -d internal/tool/regen-fixtures.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
go run ./cmd/intercall-go export \
  --out "$tmp/export" --package exportfixture \
  --interface "$tmp/export.intercall" \
  ./internal/tool/exportfixture/prov
cp "$tmp/export/binding_gen.go" internal/tool/exportfixture/binding_gen.go
cp "$tmp/export.intercall" internal/tool/testdata/export/export.intercall
go run ./cmd/intercall-go import \
  --out "$tmp/import" --package importfixture \
  internal/tool/testdata/import/import.intercall
cp "$tmp/import/binding_gen.go" internal/tool/importfixture/binding_gen.go
rm -rf "$tmp"
trap - EXIT
```

`internal/tool/fixture/codec_gen.go` has no CLI regeneration path. If its
emitter or `internal/tool/testdata/fixture.intercall` changes, run this one-shot
same-package maintenance helper:

```sh
helper=internal/tool/regenerate_codec_fixture_test.go
trap 'rm -f "$helper"' EXIT
cat >"$helper" <<'EOF'
package tool

import (
    "os"
    "testing"
)

func TestRegenerateCodecFixture(t *testing.T) {
    src, err := generateCodecFixture()
    if err != nil {
        t.Fatal(err)
    }
    if err := os.WriteFile(fixtureGenPath, src, 0o644); err != nil {
        t.Fatal(err)
    }
}
EOF
gofmt -w "$helper"
go test ./internal/tool -run '^TestRegenerateCodecFixture$'
rm -f "$helper"
trap - EXIT
```

Do not keep this helper or turn the durable comparison test into an update-mode
test.

After regeneration, run the corresponding fixture tests:

```sh
go test ./internal/tool -run \
  '^(TestGeneratedCodecFixtureCompiles|TestImportGeneratedFixtureCompiles|TestExportGeneratedFixtureCompiles)$'
go test ./internal/integration -run '^TestCheckedInGeneratedFixturesAreCurrent$'
```

Include all changed generated artifacts. Never weaken byte comparisons merely
to accept stale output. Artifact SHA-256 stamps hash the canonical interface
body and must remain consistent with it.

## Coding conventions

- Run `gofmt` on every changed Go file.
- Follow standard Go naming and error-wrapping conventions and the existing
  package-local style. Prefer small, explicit state transitions over abstract
  frameworks.
- Add package, exported-symbol, lifecycle, ownership, and concurrency comments
  where they communicate contracts. Keep comments synchronized with behavior.
- Keep changes narrowly scoped. Avoid unrelated cleanup, renaming, or generated
  churn.
- Use deterministic slices or sorted keys when emitting or reporting data from
  maps.
- For exported or user-visible behavior, update `SPEC.md` in the same change;
  update `GO.md` for usage changes and `../README.md` only for protocol changes.

## Tests

Place tests beside the code they exercise. Use external-package tests for public
contracts and same-package tests when private state or invariants must be
inspected. Prefer deterministic synchronization with channels and barriers over
sleep-based concurrency tests. Use `t.TempDir` for filesystem tests. Ordinary
validation tests must never mutate checked-in fixtures; the explicit one-shot
maintenance helper above is the only exception.

Start with the narrowest relevant command:

```sh
go test .
go test ./internal/syntax
go test ./internal/tool
go test ./cmd/intercall-go
go test ./internal/integration
go test -run 'TestName' ./path/to/package
```

`internal/tool` includes intentionally expensive boundary tests and can take
several minutes. In particular, `TestGoProjectionDepthLimit`,
`TestDeepImportProjectionBoundary`, and the generated-checker boundary tests
construct types at the exact 4,096-occurrence projection limit. They re-execute
the test binary with a reduced stack and exercise complete generation,
formatting, `go/types` checking, and artifact writing; pure list/record cases
also have deliberately expensive quadratic output or type-resolution behavior.
These tests run during ordinary `go test`, not only under `-race`. A normal
uncached full-suite run can therefore take several minutes, while `-race` is
slower still and uses longer deep-test subprocess timeouts. Do not impose a
short timeout on the full suite. For iteration, run the narrowest relevant
package or test; `go test -short ./...` does not currently skip these tests.
For lifecycle or race-sensitive work, repeat focused tests and run them under
the race detector, for example:

```sh
go test -count=20 .
go test -race -count=10 .
```

Run selected packages or tests during iteration rather than all race tests:
for example, use `go test -race ./transport/unixsocket` for Unix lifecycle
changes, or add `-run '^TestName$'` for one boundary. On the current
reference environment, the complete race suite took about 18 minutes with
`go test -race -timeout 30m ./...`; `internal/tool` alone took about 15
minutes. The default ten-minute package timeout is therefore insufficient for
the complete race suite. Reserve the full command for final or release
acceptance and use the longer timeout explicitly:

```sh
go test -race -timeout 30m ./...
```

Before finishing a code change, run the applicable standard gates from this
module root:

```sh
gofmt -w <changed-go-files>
go test ./...
go vet ./...
git diff --check
```

Use focused `-race` runs for concurrency and lifecycle changes. Run
`go test -race ./...` only for a final or release-wide acceptance gate, when an
explicit implementation plan requires it, or when the user requests it. If a
required race or vet gate cannot be run, state that explicitly rather than
claiming it passed. The Lua differential test may skip when Lua or LPeg is not
installed; for syntax changes, run it when available and investigate any
mismatch against `../README.md`, not by treating the Lua implementation as
normative.

## Change hygiene

- Inspect `git status` before and after work; do not overwrite unrelated user
  changes.
- Keep commits buildable and tests passing. Use short imperative commit subjects,
  usually with a package or subsystem prefix.
- Include source, tests, documentation, and regenerated fixtures required by one
  behavior change together unless an explicitly requested plan mandates a
  different boundary.
