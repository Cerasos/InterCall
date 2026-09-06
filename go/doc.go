// Package intercall is the shared runtime for InterCall-generated Go
// bindings.
//
// InterCall is an interface language and wire protocol; README.md defines
// both. Generated import and export bindings are thin, statically generated
// packages that use this runtime's opaque binding handles, callback types,
// and fixed error sentinels to communicate over a single bidirectional byte
// stream. The Go mapping, runtime, and generated-code SPI are defined in
// SPEC.md, and the CLI and connection usage walkthrough is in GO.md.
//
// A connection is constructed with NewConnection over an established
// ByteStream, bound into a context with WithConnection, retrieved by
// generated callers with ConnectionFromContext, and ended with Close and
// Wait. An external owner that must join complete request-handler lifetimes
// uses the additive WaitForHandlers method after terminal teardown; Close and
// Wait themselves retain their non-handler-waiting semantics. Raw NewConnection
// starts directly at the first InterCall frame and
// does not negotiate. NewNegotiatedClientConnection and
// NewNegotiatedServerConnection perform only the optional interface-ID
// agreement before constructing that same raw connection; they do not dial or
// listen. Close returns promptly after terminal publication without waiting
// for a blocked writer, gate waiter, handler, or stream cleanup, while
// Wait blocks until teardown and stream cleanup complete and returns the
// permanent terminal cause, which is never nil. Framing is bounded by the
// mandatory implementation-safety ceiling of exactly 64 MiB (67,108,864
// bytes) per frame payload defined in SPEC.md: the wire length is checked
// after the 24-byte header and before any payload allocation or read, and
// an over-ceiling frame is terminal ErrProtocol without consuming its
// payload. Raw NewConnection does not dial, listen, or negotiate; the
// application supplies the stream. The negotiated constructors perform their
// explicit interface-ID exchange but do not dial or listen.
//
// This package uses only the standard library.
package intercall
