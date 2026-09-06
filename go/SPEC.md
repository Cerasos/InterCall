# InterCall Go Proof of Concept

`README.md` is authoritative for the InterCall interface language and wire
protocol. This document defines the Go mapping, generator, and shared runtime. A
Go proof of concept must satisfy both documents; where they conflict,
`README.md` wins.

## Scope

The proof of concept implements every primitive, list, record, named type,
procedure, and exception form in `README.md`. It provides:

- a parser, validator, semantic-documentation model, and deterministic formatter
  for `.intercall` files;
- `intercall-go export`, which discovers tagged Go declarations from package
  patterns and writes an interface plus a generated export binding;
- `intercall-go import`, which reads an exact interface file and writes a
  generated import binding;
- statically generated callers, dispatch, codecs, and exception mappings; and
- a shared Go runtime for one bidirectional connection over an established byte
  stream.

This is a trusted-local-tooling, trusted-peer proof of concept. It has no
policy resource limits: the fixed ceilings in this document are mandatory
implementation-safety and representability bounds, not configurable policy. The
maximum accepted frame payload is exactly 64 MiB (67,108,864 bytes) as defined
under Reading, dispatch, and response validation, and the strict Go projection
has a maximum resolved type depth of exactly 4,096 occurrences as defined under
Strict Go projection depth. It still rejects malformed data, checks arithmetic,
and checks every wire `uint64` before converting it to `int`, allocating, or
iterating.

The repository is the module declared by `go.mod`. The root package
`github.com/cerasos/intercall/go` is the public runtime, `cmd/intercall-go` is the
CLI, `internal/syntax` owns interface syntax, and `internal/tool` owns Go
package discovery and generation. The runtime and syntax packages use only the
standard library. The tool uses `golang.org/x/tools/go/packages` only for
package discovery; everything else uses the standard library.

Each command writes one binding into a dedicated generator-owned Go package.
Import and export bindings are separate packages and cannot share an output
directory. Generated code may import the standard library, the root runtime,
and, for exports, provider packages. It never imports generator internals or a
third-party runtime. There is no reflection, runtime registration, handler
registry, generated client object, or user-supplied callback framework.

## Interface Processing

### Syntax model and validation

The parser accepts exactly the grammar, lexical rules, and UTF-8 rules in
`README.md`, including an empty file. Its AST contains only source structure:
declarations and nested type occurrences in source order, source spans, and the
documentation slots below. It does not contain Go names, Go objects, generated
helper names, or runtime state.

The parser, validator, documentation attachment, and canonical formatter use
Go call-stack space independent of unrestricted type nesting: every
syntax-owned walk uses an explicit stack or another bounded-call-stack
algorithm. The grammar has no nesting limit.

Protocol validation resolves earlier named-type references and checks all
scopes, reserved words, declaration keys, key zero, and key collisions. It
validates every declaration, including unreachable ones. Go projection is a
later boundary: a protocol-valid interface may still require a native-name
override or contain a form that cannot be represented by the strict Go subset.

Import and export build small command-specific generation records directly from
validated syntax and `go/types` objects. There is no second general-purpose AST,
target-neutral generation framework, descriptor schema, or plugin IR. The
syntax AST remains the source of wire order, wire names, documentation, and
source diagnostics; generation records add only the Go object, projected Go
name, and codec or dispatch facts needed to emit the binding.

### Semantic documentation

Documentation retention is semantic rather than a concrete-syntax round trip.
The AST has one optional documentation string on each declaration, procedure
parameter, record field, and type-specifier occurrence. A type occurrence
includes a declaration's underlying type, an exception payload, a procedure
return, a parameter or field type, a list element, a primitive or named
reference, and an inline record. An omitted exception payload or procedure
return has no type slot.

For interface input, eligible anchors are the first token of a declaration,
parameter, field, or type occurrence. Physical source lines are defined by LF
bytes: a CRLF sequence has one LF terminator, and a bare CR is whitespace, not
a physical line terminator. Body normalization below is independent of that
classification and still converts CRLF and bare CR to LF. A documentation group
is the maximal run of block comments in the trivia immediately before an
anchor, with no blank line within the group or between the group and anchor. A
blank line contains only spaces or tabs between physical lines. A comment after
a completed node on the same physical line is trailing and does not attach to a
later node, except that a candidate type prefix (a `type` name, an exception
name, a parameter or field name, `list`, or a procedure `}`) makes an
intervening comment eligible for that type even when the prefix follows an
earlier node on the same physical line. A comment between a parameter, field,
exception, or type-declaration name and its type anchors that type. A comment
after `list` anchors its element.
These rules apply recursively. Each comment attaches at most once; all
unattached comments are discarded by formatting.

Normalize documentation with this function:

1. Convert CRLF and bare CR to LF, and remove trailing spaces and tabs from each
   line.
2. Remove leading and trailing blank lines.
3. Remove from every nonblank line the longest spaces-and-tabs prefix shared by
   all nonblank lines.
4. Join lines with LF. An empty result means an empty slot.

Normalize each interface comment body separately, discard empty bodies, and
join the remaining bodies in a group with two LFs. Handwritten Go documentation
uses `go/ast.CommentGroup.Text()` as its input, removes the source-directive
lines defined below, and then applies the same normalization. A retained string
containing `*/` is an export error because InterCall has no escape for its block
comment terminator.

For nonempty `D`, `doc(indent, D)` emits:

- `indent + "/* " + D + " */\n"` when `D` has no LF; or
- `indent + "/*\n"`, each nonempty line as
  `indent + "    " + line + "\n"`, each empty line as `"\n"`, and finally
  `indent + "*/\n"`.

The canonical interface-body formatter is a recursive grammar printer:

- declarations remain in AST order, with one blank line between them;
- indentation is four spaces per enclosing procedure or record;
- adjacent same-line keywords and names use one ASCII space, and `{` follows its
  keyword or name by one ASCII space; there is no other horizontal whitespace
  outside indentation or documentation;
- a node's document appears immediately before the complete node;
- a type without documentation follows its prefix (`type name`, `field`,
  `parameter`, `exception name`, procedure `}`, or `list`) after one space;
- a documented type follows its prefix after LF, `doc` at the type's
  indentation, and the type at that indentation;
- nonempty records and parameter blocks put one field or parameter on each line;
- `record {}` and an empty parameter block `{}` stay inline;
- semicolons immediately follow their value or closing brace;
- output never wraps for line width; and
- an empty body is zero bytes, while a nonempty body ends in one LF.

This defines a byte result for every valid AST, including documentation on
nested type occurrences. Import never rewrites its input file; it uses this
formatter only to construct private semantic metadata.

### Safe import and re-export metadata

Generated Go source does not copy imported documentation into Go prose. An
import binding instead contains exactly one unexported constant whose value is
the unpadded RFC 4648 base64url encoding of the canonical interface body:

```go
const _intercallSemantic = "<payload-chunk>" +
	"<payload-chunk>"
```

The importer parses and validates its exact input, then formats that AST with
the canonical semantic formatter above. Formatting and unattached comments are
therefore absent, while every attached declaration, parameter, field, and nested
type document remains. Semantically equivalent formatting and unattached
comments produce identical metadata. Empty interface input has an empty value.

The base64url value is split left to right into quoted ASCII chunks of at most
4,096 bytes; every nonfinal chunk has exactly that size. A single chunk needs no
`+`, and the empty value is `""`. This bounds generated literal and line size
without another file or metadata record. A consumer requires exactly one
constant, canonical base64url, valid UTF-8, a successfully validated decoded
interface, and byte-for-byte equality between the decoded bytes and their
canonical reformatting.

Each generated named type has a machine line in its Go doc:

```go
// @intercall type <exact-wire-name>
```

On first use of a table-backed type in a marked generated import file (a
generated named type carrying a machine line), export validates the complete
marked file before consuming any row: a row is a top-level type spec carrying
exactly one valid `@intercall type <wire-name>` machine line, and the rows must
be in bijection with the decoded semantic type declarations; generated helper
and exception types without a machine line are permitted. Every malformed,
unknown, misplaced, duplicate, missing, extra, or structurally conflicting row
(including an otherwise unreached row) is an error at its exact physical
position. Export then decodes and parses `_intercallSemantic`, finds that exact
type declaration, and verifies that the generated Go type still projects to the
declaration's documentation-free wire structure. It then takes the declaration
and its complete nested documentation tree from the parsed semantic source.
Missing, duplicate, malformed, noncanonical, or structurally conflicting
metadata is an error.

The exact generated-file marker is the trust boundary for this metadata. An
unmarked handwritten `_intercallSemantic` constant is ordinary Go, and
handwritten `@intercall` lines are interpreted only by the source-directive
grammar in the Go export model. They never form generated metadata. In a marked
file, malformed machine metadata is an error rather than prose. Decoded
documentation is assigned directly to AST slots and is never rescanned as a Go
directive. This preserves every semantic slot, safely carries arbitrary Unicode
scalar values and directive-like text, and replaces per-slot selector machinery
with one private semantic value.

The `_intercallSemantic` metadata is a generation aid, not runtime binding
identity. Generated import and export bindings embed the SHA-256 digest of
this canonical body in metadata-aware binding constructors. The digest is
also the artifact stamp, but it is not a credential. Raw `NewConnection`
embeds no interface exchange and starts at the first frame; negotiated
constructors use the metadata without changing raw behavior.

## Go Export Model

### Package discovery and selection

Export operands are standard Go package patterns interpreted in the active
module or workspace and active Go build configuration. File operands and an
implicit module-wide scan are not supported. Tests and test variants are
excluded. A package directly matched by an operand is an **explicit package**;
a dependency loaded only for type checking is not. Pattern overlap is
deduplicated by canonical import path, and a pattern that matches no package is
an error.

Every explicit package must type-check and be an importable non-`main` package.
The export output directory must resolve to an importable package in the active
module or workspace. That generated package must be able to import every
provider, application exception package, and reachable named-type package.
`internal` visibility, workspace or module visibility, import cycles, and
inaccessible or unexported required declarations are errors. Active workspaces
may make packages from several modules eligible.

Ordinary third-party files recognized by Go's standard generated-file marker
are inert for source directive selection: they supply no selectable procedures,
no application exceptions, and no directive interpretations. A selected
signature may nevertheless reach a generated named type; on first use, export
validates the complete marked file under the rules defined under Safe import
and re-export metadata, together with the type's field tags, machine line,
source metadata, and type graph. The exact InterCall generated-file marker
remains the metadata trust boundary.

Only exported package-level functions with `@intercall procedure` are eligible.
With no `--include`, export selects every eligible function in every explicit
package. Repeatable `--include` values restrict that set; repeatable
`--exclude` values remove from it, so exclusion wins. A filter has the exact
form `full/import/path.Symbol` and must name an eligible function in an explicit
package. Malformed, unknown, duplicate, unexported, untagged, method, or generic
selectors are errors. Naming a symbol in both sets is valid and excludes it.

Filters affect procedures only. Every valid tagged application exception in a
nongenerated file of every explicit package belongs to the interface and is
available to every generated handler. Reachable ordinary types are computed
after procedure filtering from selected signatures and all retained exception
payloads.

### Source directives and Go documentation

InterCall directives occupy complete logical lines in Go doc comments after
comment markers and surrounding whitespace are removed:

```text
@intercall procedure [wire_name]
@intercall exception [wire_name]
@intercall type [wire_name]
@intercall param GoName wire_name
@param GoName text
@return text
```

Brackets denote an optional operand and are not literal. An optional wire name
replaces default source-to-wire conversion.

Each declaration directive applies to exactly one declared object. A
group-level InterCall declaration directive on a declaration group containing
multiple specs, or on one spec that declares multiple objects, is an error.
`@intercall procedure` applies only to an eligible function.
`@intercall exception` applies only to one exported package variable or one
exported, nongeneric, non-alias named defined type; parentheses around its
legal declaration RHS do not change exception eligibility. `@intercall type` applies exactly once to
every reachable ordinary defined type. `@intercall param` names a wire
parameter in the tagged function. `@param` supplies that parameter node's
documentation, and `@return` supplies the optional return type's documentation. The context
parameter and final `error` are not wire values and cannot be named or
documented by these directives.

After removing directives, a function's retained doc becomes procedure
documentation, a type's doc becomes type or payload-exception documentation, a
sentinel's doc becomes no-payload-exception documentation, and a struct field's
preceding doc becomes field documentation. Handwritten Go syntax has no other
type-occurrence documentation slots; those slots are empty. Generated source
recovers all slots from `_intercallSemantic` instead.

A sentinel declaration must contain one variable. Duplicate, malformed,
unknown, contradictory, misplaced, or unresolved InterCall directives are
errors. A bare `@intercall` is an error. Other tags and prose are retained as
ordinary documentation. Export checks InterCall-looking directives on every
package-level declaration in explicit packages and on every reachable type.
CLI flags never rename export-side wire declarations; directives and field tags
are the only overrides.

### Procedure signatures and wire values

A selected provider is an exported, nongeneric, nonvariadic package-level
function without a receiver. Its signature is exactly one of:

```go
func(context.Context, P1, ..., Pn) error
func(context.Context, P1, ..., Pn) (T, error)
```

The first parameter has the exact type identity `context.Context`, and the last
result is the predeclared `error` interface. Aliases resolving to those types
are accepted; defined lookalikes are not. There is at most one data result, and
every wire parameter has a nonblank Go name. The generated wrapper always
passes its handler context first. Methods, generics, variadics, and every other
result form are rejected.

The value mapping is exact:

| Go source form | InterCall form |
| --- | --- |
| `int8`...`int64`, `uint8`...`uint64` | same exact-width primitive |
| `float32`, `float64`, `string` | same primitive |
| `[]byte` | `bytes` |
| `[]uint8` | `list uint8` |
| `[]T` otherwise | `list T` |
| anonymous `struct { ... }` | inline `record { ... }` |
| tagged ordinary defined type | named type declaration |
| Go alias | resolved target, without a declaration |

Because `byte` aliases `uint8`, export follows source and alias RHS syntax at
each slice node: the predeclared spelling `byte` selects `bytes`, while
`uint8` selects `list uint8`. Thus `type B = []byte` and `type U = []uint8`
remain distinct. A defined `type B []byte` is a tagged named InterCall type whose
underlying type is `bytes`.

Every reachable ordinary defined type is preserved and must have exactly one
`@intercall type` directive. Aliases are flattened. Reachable defined types from
other packages must be exported and importable. Recursive type graphs,
including recursion through slices or anonymous records, and all generic
declarations or instantiations are rejected.

Unsupported wire values are `int`, `uint`, `uintptr`, `bool`, complex numbers,
arrays, pointers, maps, interfaces, channels, functions, `unsafe.Pointer`, and
all other unsafe forms. The predeclared `error` interface is legal only as the
mandatory final function result.

Go struct fields map in source order. Every field is required, named,
nonembedded, and exported. The only wire tag is:

```go
Field T `intercall:"wire_name"`
```

Its value is exactly one valid, nonreserved InterCall identifier. Empty values,
`-`, comma options, duplicate `intercall` keys, and malformed values are errors.
Other tag keys are ignored. Anonymous structs remain inline in all positions;
`struct{}` maps to `record {}`.

Nil slices encode as empty lists or bytes. Decoding an empty list or bytes value
produces a nonnil zero-length slice. A codec decodes a list of zero-width values
to the requested native length without per-element work.

### Strict Go projection depth

The strict Go projection has a maximum resolved type depth of exactly 4,096
occurrences. This is a native representability boundary, not a protocol grammar
rule and not a configurable resource policy. A root (a type declaration's
underlying type, an exception payload, a procedure parameter, or a procedure
return) has depth 1. Each list-element, record-field,
named-reference-to-underlying, defined-type-to-underlying, or alias-expansion
edge adds 1. Import and export run an iterative, cycle-safe preflight and
reject the first source occurrence exceeding 4,096 with a normal physical
diagnostic before any recursive mapping or emission; recursive graphs keep
their existing recursive-type diagnostics, which still own cycles.

### Names and native overrides

Default source-to-wire conversion is lower snake case. Default wire-to-Go
conversion is Pascal case for declarations and fields and camel case for
parameters. Conversion is ASCII-only and uses this fixed initialism set:

```text
ACL API ASCII CPU CSS DNS EOF GUID HTML HTTP HTTPS ID IP JSON QPS RAM RPC
SLA SMTP SQL SSH TCP TLS TTL UDP UI UID URI URL UTF8 UUID VM XML XMPP XSRF XSS
```

A canonical wire name matches `[a-z][a-z0-9]*(_[a-z][a-z0-9]*)*`. Split it at
underscores. A complete word whose uppercase spelling is in the set becomes
that initialism; every other word has only its first letter uppercased. Pascal
case concatenates the results. Camel case leaves the first wire word lowercase
and applies Pascal conversion to later words. For example:

```text
user_id       -> UserID / userID
http_url      -> HTTPURL / httpURL
https_client  -> HTTPSClient / httpsClient
utf8_value    -> UTF8Value / utf8Value
api2_client   -> Api2Client / api2Client
sha_value     -> ShaValue / shaValue
```

Any valid but noncanonical wire name requires an import `--go-name` override.

Source-to-wire conversion is the checked inverse:

1. Reject non-ASCII and underscore bytes. Scanning left to right, split before
   an uppercase letter whose predecessor is lowercase or a digit, and before the
   final uppercase letter of a run when that letter is followed by lowercase.
2. If an all-uppercase-or-digit run is one uppercase letter with optional
   trailing digits, keep it as one ordinary word. Otherwise, repeatedly take
   the longest complete fixed-initialism match at the start of the run and
   reject any remainder.
3. Lowercase the words, join them with underscores, project the result back to
   the required Pascal or camel form, and require byte-for-byte equality with
   the source identifier.

This accepts `HTTPServer`, `HTTPURL`, `HTTPSClient`, `UserID`, `Version42ID`,
`UTF8Value`, and `ShaValue`; it rejects `SHAValue`, `API2Client`, and
`User_ID`. In particular, `HTTPS` is chosen before its `HTTP` prefix in
`HTTPSClient`. A declaration directive, `@intercall param`, or field tag
bypasses this conversion for that wire name.

Go source identifiers, selector symbols, directive references, field
eligibility, and exportness follow Go's Unicode-aware lexical rules. The
default Go/wire projection remains ASCII-only, so an explicit declaration
directive, `@intercall param`, or field tag is required wherever the projection
cannot represent the source name.

All resulting wire names, FNV-0 keys, Go package names, Go declarations, struct
fields, and parameter names must be valid and collision-free in their actual
scope. Generated private helpers and import aliases use deterministic private
mangling, but public or parameter collisions are errors rather than silently
escaped or numbered. The default exception wire name converts the complete Go
identifier and never strips `Err`, `Error`, or another affix.

Import `--go-name SELECTOR=GoIdentifier` changes only a generated Go identifier.
It never changes wire names, keys, values, source metadata, or interface bytes.
Selectors use exact wire names:

```text
type:<name>
exception:<name>
procedure:<name>
procedure:<name>/param:<name>
type:<name><field-path>
exception:<name><field-path>
procedure:<name>/param:<name><field-path>
procedure:<name>/return<field-path>

<field-path> = zero or more (/element or /field:<name>) steps,
               followed by /field:<name>
```

A root selects its generated declaration. A parameter selector selects its
local name. A field path selects its final inline-record field; `/element`
enters a list element, and a nonfinal `/field:<name>` enters that field's type.
Type and exception paths begin at the underlying type or payload. Procedure
value paths begin at a parameter or return. A named reference is not traversed;
use its own `type:<name>` root. The fixed wrapper field `Payload` for a nonrecord
exception payload is not a wire field and has no override.

Declaration and field overrides must be exported Go identifiers; parameters may
be unexported. Every selector must resolve exactly once. Duplicate, unresolved,
keyword, wrong-visibility, or still-colliding overrides are errors. Fixed
runtime exceptions have no generated package symbol and cannot be overridden.

### Application exceptions

A no-payload application exception is one exported package variable assignable
to `error` with `@intercall exception`. A payload application exception is one
exported, nongeneric, non-alias named defined type `T` for which `*T` implements
`error`. The declaration RHS of `T` forms the payload under the ordinary value
mapping rules; `T` itself is not emitted as an ordinary named wire type. Thus,
for example, `type ProviderException string` exports
`exception provider_exception string;`, while a struct RHS exports its inline
record payload. Tagged ordinary types reached through the RHS remain named wire
types. The exception type cannot also be an ordinary named wire type or occur
as a procedure value.

The payload is the value of `T`, not `err.Error()`. Generated dispatch matches
a provider's nonnil error against every application exception in the
interface. It uses direct `err == error(provider.Sentinel)` comparisons and
direct `err.(*T)` assertions, never `errors.Is` or `errors.As`. A payload
exception therefore must be returned as a nonnil `*T`; a dynamic `T` value does
not match. After a successful assertion, dispatch dereferences the pointer and
encodes the value using the mapped RHS payload codec.

Exactly one match sends that exception. Zero or multiple matches, wrapped
errors, typed-nil payload pointers, and a panic during matching send
`internal_exception`. The runtime recovery boundary also maps provider panics
to `internal_exception`. A data result is ignored when the provider returns a
nonnil error. Failure to encode a success value or matched exception payload
also sends the no-payload `internal_exception`.

### Deterministic export order

Export first emits reachable ordinary named types in a stable topological
order: among types whose named dependencies have already been emitted, choose
the lexicographically smallest exact wire name. Remaining nodes with no ready
node are a recursive-type error. It then emits all exceptions by exact wire-name
byte order and all procedures by the same order. This satisfies `README.md`'s
backward-reference rule and is independent of Go map and package loading order.

## Go Import Model

Each procedure becomes an exported package function with `context.Context`
first and the same strict result forms as providers:

```go
func P(context.Context, P1, ..., Pn) error
func P(context.Context, P1, ..., Pn) (T, error)
```

The function obtains the connection from the root runtime context and calls the
runtime with its package's immutable import binding, one generated encoder
closure, and one generated response decoder. Missing context binding returns
`intercall.ErrNoConnection` without constructing either closure's wire result.
The runtime invokes the encoder only after local validation, as specified below.

The inverse value mapping uses `[]byte` for `bytes`, `[]uint8` for
`list uint8`, defined exported Go types for every named declaration, and
anonymous structs for inline records in every position. Named references remain
the corresponding generated named type. Generated named types carry the exact
`@intercall type` machine line, generated fields carry exact `intercall` tags,
and `_intercallSemantic` retains the canonical imported declarations and every
semantic documentation slot.

A no-payload application exception becomes an exported sentinel whose `Error`
string is exactly the wire name. An inline-record payload becomes an exported
named error struct with the record fields directly, including a distinct
zero-field type for `record {}`. Every other payload becomes an exported named
error struct with one field:

```go
Payload <mapped-payload-type>
```

Payload exceptions are returned as unwrapped, nonnil pointers. Their `Error`
method returns the exact wire name regardless of payload or documentation.

The three fixed runtime exceptions, when present with their required shape, map
to root-runtime sentinels instead of generated symbols. An interface may omit
them. Import rejects a fixed name used by another declaration kind or with a
payload.

## Generated Binding SPI and Runtime

### Immutable binding pair

The public byte-stream boundary is:

```go
type ByteStream interface {
	io.Reader
	io.Writer
	io.Closer
}
```

The stream must allow one read and one write concurrently, make `Close` unblock
both, deliver bytes reliably and in order, and begin at the first InterCall
frame. EOF and either half-close terminate the whole connection. The runtime
does not dial, listen, negotiate, or assign initiator and acceptor roles.

Generated packages expose one process-local binding value:

```go
// Generated export package.
func ExportBinding() intercall.ExportBinding

// Generated import package.
func ImportBinding() intercall.ImportBinding
```

The values are opaque handles containing an unexported pointer to immutable,
non-zero-sized runtime identity state; a private byte is sufficient. Export
state also contains its dispatch function. A binding may additionally carry a
32-byte `InterfaceID` for a later interface-agreement step; that metadata is
not process-local handle identity and is not an authentication credential.
Each package constructs its handle once. Copying a handle copies the pointer
and retains identity. Independently constructed export or import handles have
distinct state addresses, and the nil-pointer zero value is invalid. The
non-zero size is required because Go may make pointers to distinct zero-sized
variables equal. This makes ordinary value copies harmless and avoids
descriptor self-pointers, constructor registries, digests, and
copied-descriptor states. Any number of connections may share a binding
concurrently.

The complete public runtime and generated-code bridge is:

```go
type Dispatch func(
	context.Context,
	uint64, // procedure key
	[]byte, // complete owned request payload
) (uint64, []byte) // exception key and complete owned response payload

type RequestEncoder func() ([]byte, error)

type ResponseDecoder func(
	uint64, // exception key
	[]byte, // complete owned response payload
) error

type InterfaceID [32]byte

func NewExportBinding(Dispatch) (ExportBinding, error)
func NewExportBindingWithInterfaceID(Dispatch, InterfaceID) (ExportBinding, error)
func NewImportBinding() ImportBinding
func NewImportBindingWithInterfaceID(InterfaceID) ImportBinding
func EmptyExportBinding() ExportBinding
func EmptyImportBinding() ImportBinding

func (ExportBinding) InterfaceID() (InterfaceID, bool)
func (ImportBinding) InterfaceID() (InterfaceID, bool)

func NewConnection(
	context.Context,
	ByteStream,
	ExportBinding,
	ImportBinding,
) (*Connection, error)

func NewNegotiatedClientConnection(
	context.Context,
	ByteStream,
	ExportBinding,
	ImportBinding,
) (*Connection, error)
func NewNegotiatedServerConnection(
	context.Context,
	ByteStream,
	ExportBinding,
	ImportBinding,
) (*Connection, error)

func (c *Connection) Call(
	context.Context,
	ImportBinding,
	uint64, // procedure key
	RequestEncoder,
	ResponseDecoder,
) error
func (c *Connection) Wait() error
func (c *Connection) WaitForHandlers() error
func (c *Connection) Close() error

func WithConnection(context.Context, *Connection) context.Context
func ConnectionFromContext(context.Context) (*Connection, error)
```

`Dispatch`, `RequestEncoder`, `ResponseDecoder`, and `Call` are generated-code
SPI, not application callbacks or a handler registry. A request encoder returns
one complete owned payload and is invoked at most once under the ordering below.

`NewExportBinding` and `NewImportBinding` allocate fresh non-zero-sized
process-local identity state without interface metadata. The metadata-aware
constructors allocate the same kind of fresh state and record the supplied
`InterfaceID`, including an all-zero value. The accessors return the ID and
`true` when metadata is present; zero bindings and legacy constructors return
the zero ID and `false`. The metadata does not affect binding equality or
`Connection.Call`'s process-local import-handle check. `NewExportBinding` and
`NewExportBindingWithInterfaceID` both reject a nil dispatch function with
`ErrInvalidArgument`.

`EmptyExportBinding` and `EmptyImportBinding` return process-wide singleton
handles for the canonical no-procedure interface whose body is exactly:

```text
exception internal_exception;

exception invalid_arguments;

exception procedure_not_found;
```

Both carry that body's SHA-256 `InterfaceID`. The empty export dispatch
returns `procedure_not_found` with an empty payload for every complete
request. Empty means no callable procedures; it is not a zero-byte interface.
Copies returned by the accessors retain the singleton's process-local handle
identity.

`NewConnection` rejects a nil context or stream interface and zero bindings
before taking ownership of the stream. It also returns an already available
`ctx.Err()` before ownership. On success, it stores exactly one export and one
import handle for the connection; neither can change. The raw constructor

does not require, inspect, or exchange interface metadata.

`NewNegotiatedClientConnection` and `NewNegotiatedServerConnection` require
nonzero bindings with present interface IDs. They take ownership only after
local validation and an already-available context check, then perform one
setup exchange before calling the same raw connection constructor. The client
writes exactly its import/expected-server ID, the server reads and compares
it to its export ID, and only after a match writes exactly its
import/expected-client ID. The client then reads and compares that ID to its
export ID. No export ID, magic, version, length, acknowledgment, or
interface-document record is sent. A mismatch wraps `ErrInterfaceMismatch`;
missing metadata wraps `ErrInvalidArgument`, and setup transport errors retain
underlying error identity. The setup phase uses the earlier caller deadline
or ten seconds. The original context, not the temporary setup context, owns
the resulting connection. On setup failure the owned stream is closed once
and cleanup is complete before the constructor returns.

There is no generated `Run`, descriptor callback layer, startup state, or
startup wait. `NewConnection` completely initializes the connection and starts
its sole receive-loop goroutine before returning. A call is therefore either on
an active connection or returns its terminal cause. Generated callers pass
their import singleton to `Call`, which compares handle identity and checks
terminal and call-context state before invoking the request encoder. The export
handle needs no later check because construction already fixed it.

`WithConnection` uses one private root-runtime key and replaces any earlier
binding. It follows `context.WithValue`: a nil parent or connection panics.
`ConnectionFromContext` returns `ErrNoConnection` unless a nonnil connection is
bound. Handler contexts are derived from the connection context, bound with the
same function, and canceled when the handler finishes or the connection
terminates.

### Lifecycle and local errors

A successfully constructed connection has only two lifecycle conditions:
active and terminal. In addition to the receive loop, construction starts one
context-observer goroutine. It waits for either the construction context's
`Done` channel or the connection's terminal-selection channel. Context
cancellation attempts terminal selection with the exact `ctx.Err()` value;
the runtime never uses `context.Cause` or wraps that value. A nil `Done` channel,
as from `context.Background`, simply disables the context case.

Explicit `Close`, a read or write failure, EOF or half-close, and a terminal
protocol error use the same lock-protected selection. The first selected error
is permanent and closes the terminal-selection channel. Publication under the
state lock fixes the cause and transfers every existing pending entry away from
later response or per-call-cancellation claims; closing the stream exactly once,
canceling handler contexts, and delivering the terminal completion of each
transferred entry may then proceed asynchronously. A stream cleanup error never
replaces or joins the published cause. If another event wins, the
terminal-selection channel wakes the context observer; it rechecks terminal
state under the same lock and exits without attempting a new cause. If context
cancellation wins, the observer completes selection and teardown before
exiting.

`Close` selects `ErrClosed` if needed and otherwise does nothing; in either
case, terminal publication completes under the state lock before `Close`
returns. `Close` never waits for the receive loop, observer, handlers, blocked
gate waiters, or stream cleanup. `Wait` waits for the receive loop, complete
terminal teardown and stream cleanup, and context-observer exit, then returns
the permanent terminal cause; it never returns nil. `Wait` retains this
non-handler-waiting contract. The additive owner-side `WaitForHandlers` method
first performs that existing `Wait` completion, then waits for one per-connection
handler `sync.WaitGroup`, and returns the same permanent terminal cause. The
receive loop stops before the zero-counter wait; every normal handler is counted
before its goroutine launch, and a deferred same-ID generation is counted before
launch while its parent handler is still counted. The count covers the complete
handler lifetime: dispatch argument decoding, provider execution, response
encoding, frame construction, write admission and write, or terminal discard.
`WaitForHandlers` has no grace period or cancellation override, so a handler
that ignores cancellation may delay it indefinitely. It is for an external
connection owner and must not be called from an active handler or stream
cleanup; neither `Close` nor `Wait` calls it. Thus EOF under
`context.Background` cannot strand the observer, `context.WithCancelCause`
yields exactly `context.Canceled` rather than its cause, a cause-bearing deadline
yields `context.DeadlineExceeded`, and Close/cancellation races retain whichever
exact cause wins the common selection lock. Terminal state still prevents a
handler from beginning a later response write.

The root package exports only these local classifications:

```go
ErrInvalidArgument
ErrNoConnection
ErrBindingMismatch
ErrClosed
ErrRequestIDsExhausted
ErrProtocol
ErrInterfaceMismatch
```

It also exports the three wire-exception sentinels listed below. Sentinels work
with direct comparison and `errors.Is`. The constructors and methods above, and
`ConnectionFromContext`, return or wrap `ErrInvalidArgument` for a nil dispatch,
context, receiver, stream interface, encoder, or decoder, a zero binding passed
to `NewConnection`, or a zero procedure key. (`WithConnection` has the explicit
panic contract above.) A zero or different import handle passed to `Call` on a
valid connection returns `ErrBindingMismatch`. Argument and binding validation
occurs before terminal-state inspection. After validation, an already selected
terminal cause wins over an already canceled call context; otherwise the exact
context error wins. Generated paths always pass valid arguments. A nil payload
returned by a successful encoder is a valid empty payload.

A terminal transport error adds a short operation prefix and wraps the stream
error, preserving it for `errors.Is` and `errors.As`. A terminal framing or
matched-response error wraps `ErrProtocol`; request IDs and detailed codec text
may appear in its noncontractual message. There are no exported operation enums,
structured diagnostic error records, loggers, panic hooks, or stack hooks.

A per-call context cancellation returns that context's exact
`context.Canceled` or `context.DeadlineExceeded` when cancellation claims the
call. It does not terminate the connection. Concurrent connection-terminal
events use first-cause selection; concurrent response, per-call cancellation,
and terminal outcomes use the pending-call ownership rule below.

### Reading, dispatch, and response validation

The receive loop is the only reader. It uses full-read semantics for each
24-byte header and then allocates and reads the complete payload after checking
its wire length against native `int` and against the maximum accepted frame
payload of exactly 64 MiB (67,108,864 bytes). The ceiling is a mandatory
implementation-safety bound, not configurable policy: it is checked after the
header and before conversion or allocation, and a larger frame is terminal
`ErrProtocol` without consuming its payload. An incomplete header or payload is
a transport failure; impossible native size, an over-ceiling payload, or
structural frame failure is a protocol error. Decoders receive only the owned
payload slice and cannot consume a later frame.

Each request transfers its complete payload to one new, unbounded handler
goroutine. Before starting it, the receive loop reserves the incoming request ID
in an active set and increments the connection's handler lifetime counter before
launching the goroutine. Request admission is ordered against terminal
publication: a buffered frame is never dispatched after terminal has won.
Incoming and outgoing ID spaces are independent. A deferred same-ID generation
is counted before its goroutine launch while the current handler remains
counted, so an owner-side zero-counter wait cannot race a valid launch.

README permits a peer to reuse a request ID as soon as it has received the
complete prior response, but `ByteStream` exposes no peer-delivery
acknowledgement, so the runtime orders reuse locally. A duplicate incoming ID
observed before the prior response enters write admission is a terminal
protocol error. A duplicate observed while that response write is active is
fully buffered and reserved as one deferred next generation without parking the
sole receive loop; a further same-ID request cannot also queue. A successful
response write admits the deferred request even if it arrived before the final
local `Write` returned; a write failure or terminal selection discards it. The
runtime is not required to detect a peer's early reuse once the prior response
write is active.

Generated dispatch is a static switch on procedure key. An unknown key receives
`procedure_not_found` after its payload has been buffered. A known procedure
whose arguments are malformed or leave trailing bytes receives
`invalid_arguments`, without invoking the provider. One runtime recovery around
the complete dispatch maps every escaped panic to `internal_exception`. The
handler fully encodes its selected response before entering the shared write
gate.

A response is completely buffered before lookup. If its ID has no pending entry,
the runtime consumes and ignores its exception key and payload as opaque bytes.
For a pending ID, the receive loop removes the entry, thereby claiming the call,
and invokes that call's generated decoder in the receive goroutine. Nil means
the decoder accepted one declared exception or success value, consumed the
payload exactly, and stored the typed result in its closure. It then completes
the removed entry successfully. An error or panic terminates the connection and
completes that entry with the permanent terminal cause. Consequently, unknown
exception keys, invalid values, noncanonical NaNs, wrong zero-width payloads,
and trailing bytes in a matched response always terminate the connection;
canceled or otherwise unmatched responses remain opaque as required by
`README.md`.

The runtime never reuses a frame buffer. A generated decoder may retain owned
byte subslices in the result, and channel or lock synchronization makes closure
writes visible before the generated caller returns.

### Calls, pending ownership, and IDs

Outgoing IDs increase monotonically from `0` through
`0x7fffffffffffffff` and are never reused, including after completion or local
cancellation. After allocating the final ID, the next call returns
`ErrRequestIDsExhausted` without writing a frame. Each peer allocates
independently.

The generated caller passes a request-encoder closure to `Call`. `Call` then:

1. validates its receiver, context, exact import identity, procedure key,
   encoder, and decoder;
2. returns an already selected terminal cause or already available `ctx.Err()`;
3. invokes the encoder exactly once to obtain one complete owned payload;
4. returns the encoder's exact error, if any, without allocating an ID,
   constructing a frame, or entering the write gate;
5. rechecks terminal state and `ctx.Err()`, builds the owned contiguous frame,
   and waits on the write gate while allowing either to win;
6. rechecks both under the connection lock, allocates an ID, and inserts one
   pending entry immediately before write admission;
7. writes the whole buffered frame while holding the gate; and
8. after write completion, waits for response, per-call cancellation, or
   connection termination.

A binding mismatch, terminal connection, or cancellation visible at the
pre-encode checks never invokes the encoder. If termination or cancellation
happens while a successful encoder runs, the post-encode check returns it
without an ID or frame. An encoder error itself returns directly as step 4
specifies. No ID is allocated while waiting for the write gate. Insertion and
write admission are one lock-protected action; that admission point defines the start
of the write even if terminal teardown closes the stream before the subsequent
`Write` call enters it. After admission, the per-call context cannot interrupt
the write, close the stream, or claim the pending entry; this proof of concept
has no transport cancellation. A response or connection termination may claim
the entry during the full-duplex write. A write failure terminates the
connection. If a response already removed the entry, that response remains this
call's outcome; otherwise terminal teardown claims it.

The pending map is the state machine: presence means the admitted request is
eligible for one outcome, and removal transfers exclusive ownership to exactly
one response, per-call cancellation, or terminal teardown. There are no
registered/writing/waiting/claimed enum states or tombstones. Cancellation
removes the entry and permanently retires its ID, so a later response is
unmatched and opaque.

There is no cancellation frame, and local cancellation does not imply that the
remote handler stops.

### Frame writing and generated codecs

Requests and responses share one connection-wide write gate. Every value is
append-encoded into an owned payload, then combined with its header before the
gate is acquired. A gate wait observes terminal selection and, for outgoing
calls, the call context's `Done`; the connection state lock is never held while
waiting for the gate or while calling stream `Write` or `Close`. While holding
the gate, the runtime writes until the complete frame is accepted or the writer
reports an error, an invalid byte count, or no progress. It never interleaves
frames. Any error after a partial frame is
terminal. Encoding cannot fail after a frame write begins, and mutable provider
values are observed in one encoding pass.

A handler's incoming ID remains active until its complete response write
succeeds; a duplicate observed during that write becomes the deferred next
generation defined under Reading, dispatch, and response validation. A handler
waiting for the gate abandons its response after terminal selection; a handler
already writing is unblocked by stream closure. The handler lifetime counter is
released only after this full path, including deferred-generation admission,
has finished.

Generated append encoders and bounded decoders implement the exact wire rules in
`README.md`, including:

- little-endian exact-width two's-complement integers;
- canonical output NaNs and rejection of every other NaN encoding;
- UTF-8 validation when encoding Go strings and decoding wire strings;
- checked lengths, counts, additions, and multiplications before conversion,
  allocation, slicing, or iteration;
- declaration-order records without padding;
- exact request and matched-response payload exhaustion; and
- native-length allocation but no per-element loop for zero-width list elements.

There is no pooling that exposes buffer reuse and no configurable policy limit
below native representability and available payload bytes; the only fixed
ceilings are the 64 MiB frame-payload safety bound and the strict Go
projection's 4,096-occurrence depth boundary.

## Fixed Go Runtime Exceptions

The proof of concept fixes these no-payload wire exceptions:

| Name | Key |
| --- | --- |
| `procedure_not_found` | `0x970e76fcc5e2dacb` |
| `invalid_arguments` | `0x3f5fc972f8477b07` |
| `internal_exception` | `0x1aaec22e85996f50` |

Export inserts all three into every interface. Their names are reserved across
the global InterCall declaration namespace. Import accepts them only as
no-payload exception declarations and maps them, when present, to shared root
sentinels:

```go
ErrProcedureNotFound
ErrInvalidArguments
ErrInternalException
```

Each sentinel's `Error` string is its exact wire name. Runtime conditions, not
provider matching, select these exceptions. A fully framed unknown request gets
`procedure_not_found`; malformed or trailing arguments get `invalid_arguments`;
and provider, matching, or response-encoding failures get
`internal_exception`. A frame that cannot be safely buffered, including a
payload above the 64 MiB ceiling, is terminal `ErrProtocol`. Every malformed
matched response is terminal.

## CLI and Generated Artifacts

### Commands

```text
intercall-go export --out DIR --interface FILE [--package NAME]
    [--include full/import/path.Symbol]...
    [--exclude full/import/path.Symbol]...
    PACKAGE_PATTERN...

intercall-go import --out DIR [--package NAME]
    [--go-name SELECTOR=GoIdentifier]...
    INTERFACE_FILE
```

Export requires at least one package pattern and distinct binding and interface
targets. Import requires exactly one file and reads its exact bytes; stdin is
not supported. The shown filter and naming flags are repeatable.

`--package` sets the generated package name. Without it, an existing owned
binding's package clause wins; a new output uses the output directory's base
name. The name must match `[A-Za-z_][A-Za-z0-9_]*`, and cannot be `_`, `main`,
or a Go keyword. The tool never sanitizes it. An explicit name must equal an
existing owned binding's package name.

### One-file ownership and safe replacement

Every binding is one file named `binding_gen.go`; file partitioning is not a
configuration or generator decision. Its first two lines have this exact form:

```go
// Code generated by intercall-go; DO NOT EDIT.
// intercall-go binding: import sha256:<artifact-id>
```

The second line says `export` for an export binding. `<artifact-id>` is the
lowercase 64-hex-digit SHA-256 digest of the canonical interface body represented
by the binding. It is a local update stamp only: the runtime does not store,
compare, or exchange it. A fixed file removes the manifest, file-list schema,
stale-file deletion, and update transaction. The generator never deletes a
path.

The exported interface starts with this exact ownership form followed by one
blank line and the canonical interface body:

```text
/* Code generated by intercall-go; artifact sha256:<artifact-id>; DO NOT EDIT. */
```

Its stamp hashes the body, not the ownership line. The blank line leaves the
marker semantically unattached, so importing the file discards the marker when
constructing `_intercallSemantic`.

Before writing, the CLI validates all source, interface, projection, and
generated bytes in memory and parses and type-checks the complete generated Go
in memory before any filesystem mutation. Generated Go must have complete
collision-free package and local scopes. Export checking reuses the exact
`*types.Package` identities from the one combined discovery load. Import
checking may use one synthetic runtime SPI package only when a durable parity
test compares every modeled exported generated-code bridge object and signature
with the actual root package. Type checking uses the standard library. It
creates `--out` if necessary; after that, the interface target's parent must
exist. It resolves both target parents through
the host filesystem and operates on the resolved directories. A target leaf is
inspected with non-following file status; a symlink, directory, device, or other
nonregular leaf is an error. The interface target must not have a `.go`
filename and must not be the generated Go target under the host filesystem's
filename equivalence.

The output directory may contain non-Go entries, which are always preserved. It
may contain no Go file or Go-named nonregular entry except
`binding_gen.go`. An existing `binding_gen.go` is replaceable only when its two
ownership lines and artifact stamp have valid syntax and its mode and package
match this invocation. An existing interface target is replaceable only when it
is a regular nonsymlink file with a valid ownership line and its stamp matches
its canonical body. Every other collision is an error. Thus the tool never overwrites a
handwritten or differently generated Go file and never overwrites an unmarked
interface file.

The CLI formats and parses staged Go, reparses the staged interface, and writes
temporary files in the destination directories before replacement. It replaces
owned targets by rename rather than truncating them; hard links to an old target
therefore retain the old inode and bytes. If the host cannot replace an existing
file by rename, the command fails without first deleting it. An unchanged target
is not replaced.

Export has two independent owned targets, not a speculative cross-filesystem
transaction. Before replacement, it compares the valid existing binding and
interface stamps. Different stamps, or exactly one missing owned target, mean a
prior update was interrupted; this is recoverable ownership state, not
permission to touch an unowned collision. After both new targets are staged and
validated, the tool replaces `binding_gen.go` and then the interface. A process
or filesystem failure may again leave differing stamps, and the next successful
invocation deterministically repairs both without a manifest or deletion. It
never endangers an unowned file. Concurrent hostile filesystem mutation is
outside the trusted local CLI threat model.

For identical semantic inputs and Go build configuration, generated interface
and Go bytes are identical. On import, differences only in formatting and
unattached comments are not semantic input differences. Output contains no
timestamp, absolute source path, temporary path, or map-order-dependent data.
Generated artifacts are intended to be checked into version control.

### Diagnostics

Source diagnostics use `path:line:column: message` with one-based `go/token`
physical line and byte-column semantics. Position parsing accepts both
`file:line` and `file:line:column`, scans numeric suffixes from the right so
colons in filenames are preserved, and defaults a missing column to 1. `//line`
directives do not rewrite positions. Interface positions come from byte offsets
in the exact input; invalid UTF-8 points to its first invalid byte, and EOF uses
offset `len(input)` under the same rules. A physical Go source under the package
directory uses the slash-normalized package-relative path under its canonical
import path. An external compiler-generated source uses
`<import-path>/.intercall-generated/<base-name>`; duplicate external base names
are a package-load invariant error rather than silently conflated. Errors
without a source span use line 1, column 1 of the relevant operand.

When a phase produces several diagnostics, the CLI sorts them by logical path,
line, column, and message. It never reports a staging path. Source validation,
generated-content validation, and generated-Go type checking finish before
output-directory creation; ownership checks then finish before target-file
creation or replacement. Any validation error emits no generated file.

## Native Go transports

The Go module provides optional native transports under
`transport/unixsocket` and `transport/websocket`. These packages are not part
of the root runtime and do not change frames, values, calls, or connection
lifecycle semantics. Raw `NewConnection` remains metadata-free and starts at
the first frame; the negotiated constructors perform only the two-record
interface agreement described below.

Generated bindings carry the SHA-256 digest of the canonical interface body as
an `InterfaceID`. It is metadata and an early contract-mismatch detector, not a
credential. In the client-first exchange, the client writes its import (the
expected server-export) ID; the server compares it with its export ID and then
writes its import (the expected client-export) ID; the client compares that
with its export ID. No export ID, version, magic, acknowledgment, or interface
document is sent. A mismatch wraps `ErrInterfaceMismatch`; missing metadata is
local `ErrInvalidArgument`.

Unix sockets use filesystem-backed `SOCK_STREAM` paths. `ListenStream` refuses
any existing leaf, anchors relative paths once, defaults to mode `0600`, and
removes only the socket identity it created. `Dial`, `AcceptConnection`, and
`ListenAndServe` use negotiated bindings; low-level stream APIs leave
authentication and negotiation to the application. `ListenAndServe` returns
`unixsocket.ErrServerClosed` after context-driven shutdown.

WebSockets use `github.com/coder/websocket` as a binary continuous byte
stream. Message boundaries are not InterCall frame boundaries and text
messages are rejected. The default message limit is 67,108,888 bytes (64 MiB
plus the 24-byte frame header); compression is disabled and same-origin
checking is enabled by default. `NewHandler` is intended to be wrapped by
ordinary HTTP authentication middleware. The convenience server is plain HTTP
for loopback deployments such as cloudflared and returns `http.ErrServerClosed`
on orderly context shutdown; it does not terminate TLS.

The proof of concept does not include:

- TypeScript or any other non-Go binding;
- WebTransport or independent per-call stream adapters;
- a mandatory handshake for raw `NewConnection`, or any claim that
  interface IDs authenticate or authorize a peer;
- authentication, authorization, policy, or procedure whitelists;
- configurable policy resource limits; the fixed 64 MiB frame-payload safety
  ceiling and the strict Go projection's 4,096-occurrence depth boundary are
  mandatory implementation bounds, not policy;
- TLS certificate loading or termination, reconnect, retry, pooling, or
  session resumption;
- transport-level or wire-level cancellation;
- streaming values, parameters, or results; or
- compatibility promises for Go toolchains older than the version in `go.mod`.
