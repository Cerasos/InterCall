package tool

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/cerasos/intercall/go"
	"github.com/cerasos/intercall/go/internal/syntax"
	exportfixture "github.com/cerasos/intercall/go/internal/tool/exportfixture"
)

// This file tests the compiled export fixture as generated code: the
// immutable export binding singleton, the static procedure key switch
// over real provider calls (success, every direct exception payload,
// typed-nil and multiple matches, wrapped errors, encoding failures,
// malformed and trailing arguments, unknown keys), the runtime's one
// recovery around the complete dispatch (provider and matching panics
// select internal_exception), and the direct request decoder
// acceptance table.

// TestExportBindingIdentity verifies the immutable export binding
// singleton: ExportBinding returns the same non-zero handle on every
// call, the handle's dispatch is fixed at construction, and
// independently constructed handles are distinct.
func TestExportBindingIdentity(t *testing.T) {
	a := exportfixture.ExportBinding()
	b := exportfixture.ExportBinding()
	if a != b {
		t.Fatal("ExportBinding returned different handles on repeated calls")
	}
	if a == (intercall.ExportBinding{}) {
		t.Fatal("the generated export binding handle is zero")
	}
	fresh, err := intercall.NewExportBinding(func(context.Context, uint64, []byte) (uint64, []byte) {
		return 0, nil
	})
	if err != nil {
		t.Fatalf("NewExportBinding: %v", err)
	}
	if a == fresh {
		t.Fatal("an independently constructed export handle is not distinct")
	}
	if exportfixture.ExportBindingHandle != a {
		t.Fatal("the exported handle does not match the generated singleton")
	}
}

// callDispatch runs the generated dispatch over one encoded request
// payload.
func callDispatch(t *testing.T, key uint64, payload []byte) (uint64, []byte) {
	t.Helper()
	excKey, resp := exportfixture.Dispatch(context.Background(), key, payload)
	return excKey, resp
}

// TestExportDispatchDirect drives the generated dispatch directly over
// every procedure and every exception outcome, without a connection:
// success values and each direct exception payload, the typed-nil and
// multiple-match fallbacks, wrapped errors, encoding failures,
// malformed and trailing arguments, and unknown keys.
func TestExportDispatchDirect(t *testing.T) {
	echoKey := syntax.ProcedureKey("echo")
	failed := exceptionKey("failed")
	denied := exceptionKey("denied")
	empty := exceptionKey("empty")
	internal := exceptionKey("internal_exception")
	invalidArgs := exceptionKey("invalid_arguments")
	procNotFound := exceptionKey("procedure_not_found")

	t.Run("echo success", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "hello, intercall"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != 0 {
			t.Fatalf("echo = exception %#x, want success", excKey)
		}
		out, rest, err := exportfixture.Codecs.DecodeString(resp)
		if err != nil || len(rest) != 0 {
			t.Fatalf("decoding the echo response: %v, rest %d", err, len(rest))
		}
		if out != "hello, intercall" {
			t.Fatalf("echo response = %q, want the encoded input", out)
		}
	})

	t.Run("add success", func(t *testing.T) {
		payload := append(mustVec(exportfixture.Codecs.EncodeInt64(nil, 40)), mustVec(exportfixture.Codecs.EncodeInt64(nil, 2))...)
		excKey, resp := callDispatch(t, syntax.ProcedureKey("add"), payload)
		if excKey != 0 {
			t.Fatalf("add = exception %#x, want success", excKey)
		}
		out, rest, err := exportfixture.Codecs.DecodeInt64(resp)
		if err != nil || len(rest) != 0 || out != 42 {
			t.Fatalf("add response = %d, rest %d, err %v; want 42", out, len(rest), err)
		}
	})

	t.Run("paint success", func(t *testing.T) {
		origin := mustVec(exportfixture.Codecs.EncodePaintOrigin(nil, exportfixture.PaintOrigin{X: 1, Y: 2}))
		size := mustVec(exportfixture.Codecs.EncodePaintSize(nil, exportfixture.PaintSize{Width: 3, Height: 4}))
		payload := append(origin, size...)
		excKey, resp := callDispatch(t, syntax.ProcedureKey("paint"), payload)
		if excKey != 0 {
			t.Fatalf("paint = exception %#x, want success", excKey)
		}
		out, rest, err := exportfixture.Codecs.DecodePaintResult(resp)
		if err != nil || len(rest) != 0 {
			t.Fatalf("decoding the paint response: %v, rest %d", err, len(rest))
		}
		if out != (exportfixture.PaintResult{Width: 3, Height: 4}) {
			t.Fatalf("paint response = %+v, want the size", out)
		}
	})

	t.Run("fetch named types", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeUserID(nil, 7))
		excKey, resp := callDispatch(t, syntax.ProcedureKey("fetch"), payload)
		if excKey != 0 {
			t.Fatalf("fetch = exception %#x, want success", excKey)
		}
		out, rest, err := exportfixture.Codecs.DecodePoint(resp)
		if err != nil || len(rest) != 0 {
			t.Fatalf("decoding the fetch response: %v, rest %d", err, len(rest))
		}
		if out.X != 7 || out.Y != 8 {
			t.Fatalf("fetch response = %+v, want the point of user 7", out)
		}
	})

	t.Run("sample and wave", func(t *testing.T) {
		data := mustVec(exportfixture.Codecs.EncodeBytes(nil, []byte{1, 2, 3}))
		channel := mustVec(exportfixture.Codecs.EncodeUint8(nil, 9))
		excKey, resp := callDispatch(t, syntax.ProcedureKey("sample"), append(data, channel...))
		if excKey != 0 {
			t.Fatalf("sample = exception %#x, want success", excKey)
		}
		out, rest, err := exportfixture.Codecs.DecodeBytes(resp)
		if err != nil || len(rest) != 0 || len(out) != 3 || out[0] != 1 {
			t.Fatalf("sample response = %v, rest %d, err %v", out, len(rest), err)
		}

		samples := mustVec(exportfixture.Codecs.EncodeListUint8(nil, []uint8{4, 5, 6}))
		excKey, resp = callDispatch(t, syntax.ProcedureKey("wave"), samples)
		if excKey != 0 {
			t.Fatalf("wave = exception %#x, want success", excKey)
		}
		count, rest, err := exportfixture.Codecs.DecodeUint32(resp)
		if err != nil || len(rest) != 0 || count != 3 {
			t.Fatalf("wave response = %d, rest %d, err %v; want 3", count, len(rest), err)
		}
	})

	t.Run("ping success", func(t *testing.T) {
		excKey, resp := callDispatch(t, syntax.ProcedureKey("ping"), nil)
		if excKey != 0 || len(resp) != 0 {
			t.Fatalf("ping = %#x, %d bytes; want success with an empty payload", excKey, len(resp))
		}
	})

	t.Run("fixed runtime sentinels", func(t *testing.T) {
		for _, test := range []struct {
			value string
			key   uint64
		}{
			{"invalid_arguments", invalidArgs},
			{"procedure_not_found", procNotFound},
			{"internal_exception", internal},
		} {
			payload := mustVec(exportfixture.Codecs.EncodeString(nil, test.value))
			excKey, resp := callDispatch(t, echoKey, payload)
			if excKey != test.key || len(resp) != 0 {
				t.Fatalf("%s = %#x, %d bytes; want %#x", test.value, excKey, len(resp), test.key)
			}
		}
	})

	t.Run("denied sentinel", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "denied"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != denied || len(resp) != 0 {
			t.Fatalf("denied = %#x, %d bytes; want the denied sentinel with an empty payload", excKey, len(resp))
		}
	})

	t.Run("failed payload", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "failed"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != failed {
			t.Fatalf("failed = %#x, want the failed exception", excKey)
		}
		got, rest, err := exportfixture.Codecs.DecodeFailedPayload(resp)
		if err != nil || len(rest) != 0 {
			t.Fatalf("decoding the failed payload: %v, rest %d", err, len(rest))
		}
		if got.Code != 7 || got.Message != "boom" {
			t.Fatalf("failed payload = %+v, want code 7 message boom", got)
		}
	})

	t.Run("empty zero-field payload", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "empty"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != empty || len(resp) != 0 {
			t.Fatalf("empty = %#x, %d bytes; want the empty exception with an empty payload", excKey, len(resp))
		}
	})

	t.Run("multiple match", func(t *testing.T) {
		// The shared sentinel is also a payload exception value, so
		// returning it produces two direct matches and the dispatch
		// selects internal_exception.
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "shared"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != internal || len(resp) != 0 {
			t.Fatalf("shared = %#x, %d bytes; want internal_exception", excKey, len(resp))
		}
	})

	t.Run("typed nil pointer", func(t *testing.T) {
		// A typed-nil payload pointer satisfies no direct match: the
		// assertion guard requires a nonnil pointer.
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "typed_nil"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != internal || len(resp) != 0 {
			t.Fatalf("typed_nil = %#x, %d bytes; want internal_exception", excKey, len(resp))
		}
	})

	t.Run("wrapped error", func(t *testing.T) {
		// A wrapped error matches no direct equality or assertion.
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "wrapped"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != internal || len(resp) != 0 {
			t.Fatalf("wrapped = %#x, %d bytes; want internal_exception", excKey, len(resp))
		}
	})

	t.Run("encoding failure", func(t *testing.T) {
		// The provider returns a success value the encoder rejects;
		// the failure sends the no-payload internal_exception.
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "bad_utf8"))
		excKey, resp := callDispatch(t, echoKey, payload)
		if excKey != internal || len(resp) != 0 {
			t.Fatalf("bad_utf8 = %#x, %d bytes; want internal_exception", excKey, len(resp))
		}
	})

	t.Run("malformed arguments", func(t *testing.T) {
		// A truncated argument payload selects invalid_arguments
		// without invoking the provider.
		excKey, resp := callDispatch(t, echoKey, []byte{0xff, 0xfe})
		if excKey != invalidArgs || len(resp) != 0 {
			t.Fatalf("malformed echo = %#x, %d bytes; want invalid_arguments", excKey, len(resp))
		}
		excKey, resp = callDispatch(t, syntax.ProcedureKey("paint"), []byte{0x00})
		if excKey != invalidArgs || len(resp) != 0 {
			t.Fatalf("malformed paint = %#x, %d bytes; want invalid_arguments", excKey, len(resp))
		}
	})

	t.Run("trailing arguments", func(t *testing.T) {
		// A payload that decodes but leaves trailing bytes selects
		// invalid_arguments without invoking the provider.
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "hello"))
		excKey, resp := callDispatch(t, echoKey, append(payload, 0x00))
		if excKey != invalidArgs || len(resp) != 0 {
			t.Fatalf("trailing echo = %#x, %d bytes; want invalid_arguments", excKey, len(resp))
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		excKey, resp := callDispatch(t, 0xdeadbeef, []byte{1, 2, 3})
		if excKey != procNotFound || len(resp) != 0 {
			t.Fatalf("unknown key = %#x, %d bytes; want procedure_not_found", excKey, len(resp))
		}
	})
}

// exportStream is the connection end of an in-memory full-duplex byte
// stream: the connection reads request frames from requestsR and
// writes response frames to responsesW, and Close unblocks both the
// connection's receive loop and the test peer.
type exportStream struct {
	requestsR  *io.PipeReader // peer-to-connection direction
	responsesW *io.PipeWriter // connection-to-peer direction
}

func (s *exportStream) Read(p []byte) (int, error)  { return s.requestsR.Read(p) }
func (s *exportStream) Write(p []byte) (int, error) { return s.responsesW.Write(p) }
func (s *exportStream) Close() error {
	_ = s.responsesW.Close() // unblocks the peer's pending read
	return s.requestsR.CloseWithError(io.ErrClosedPipe)
}

// exportPeer is the test transport peer of the export runtime tests:
// it writes request frames to the connection and reads response
// frames back.
type exportPeer struct {
	w *io.PipeWriter // test-to-connection direction
	r *io.PipeReader // connection-to-test direction
}

func newExportDuplex() (*exportStream, *exportPeer) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	return &exportStream{requestsR: reqR, responsesW: respW},
		&exportPeer{w: reqW, r: respR}
}

// writeExportRequest writes one complete request frame.
func writeExportRequest(w io.Writer, id, procKey uint64, payload []byte) error {
	var hdr [24]byte
	binary.LittleEndian.PutUint64(hdr[0:8], id)
	binary.LittleEndian.PutUint64(hdr[8:16], procKey)
	binary.LittleEndian.PutUint64(hdr[16:24], uint64(len(payload)))
	buf := make([]byte, 0, 24+len(payload))
	buf = append(buf, hdr[:]...)
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	return err
}

// readExportResponse reads one complete response frame: the 24-byte
// header with full-read semantics and the complete payload.
func readExportResponse(r io.Reader) (id, excKey uint64, payload []byte, err error) {
	var hdr [24]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	id = binary.LittleEndian.Uint64(hdr[0:8]) & 0x7fffffffffffffff
	excKey = binary.LittleEndian.Uint64(hdr[8:16])
	n := binary.LittleEndian.Uint64(hdr[16:24])
	if n > 1<<20 {
		return 0, 0, nil, fmt.Errorf("export test: oversized response payload")
	}
	payload = make([]byte, int(n))
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, nil, err
	}
	return id, excKey, payload, nil
}

// newExportConnection constructs one connection over an in-memory
// duplex pair with the fixture's export binding and a fresh import
// binding, returning the connection and a cleanup that closes it and
// waits for complete teardown.
func newExportConnection(t *testing.T, stream intercall.ByteStream) (*intercall.Connection, func()) {
	t.Helper()
	conn, err := intercall.NewConnection(context.Background(), stream, exportfixture.ExportBinding(), intercall.NewImportBinding())
	if err != nil {
		t.Fatalf("NewConnection: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		_ = conn.Wait()
	}
	return conn, cleanup
}

// TestExportConnectionRuntime drives the generated dispatch over a
// real connection and byte stream: the runtime's one recovery around
// the complete dispatch maps a provider panic and a matching panic to
// internal_exception, an unknown key receives procedure_not_found, and
// a success value round-trips through the wire.
func TestExportConnectionRuntime(t *testing.T) {
	t.Run("provider panic", func(t *testing.T) {
		client, peer := newExportDuplex()
		_, cleanup := newExportConnection(t, client)
		defer cleanup()
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "panic"))
		if err := writeExportRequest(peer.w, 1, syntax.ProcedureKey("crash"), payload); err != nil {
			t.Fatalf("writing the crash request: %v", err)
		}
		id, excKey, resp, err := readExportResponse(peer.r)
		if err != nil {
			t.Fatalf("reading the crash response: %v", err)
		}
		if id != 1 || excKey != exceptionKey("internal_exception") || len(resp) != 0 {
			t.Fatalf("crash response = id %d, %#x, %d bytes; want the internal_exception response", id, excKey, len(resp))
		}
	})

	t.Run("matching panic", func(t *testing.T) {
		// Comparing the uncomparable error value against its own
		// sentinel panics inside the matcher; the runtime recovery
		// sends internal_exception.
		client, peer := newExportDuplex()
		_, cleanup := newExportConnection(t, client)
		defer cleanup()
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "weird"))
		if err := writeExportRequest(peer.w, 2, syntax.ProcedureKey("echo"), payload); err != nil {
			t.Fatalf("writing the weird request: %v", err)
		}
		id, excKey, resp, err := readExportResponse(peer.r)
		if err != nil {
			t.Fatalf("reading the weird response: %v", err)
		}
		if id != 2 || excKey != exceptionKey("internal_exception") || len(resp) != 0 {
			t.Fatalf("weird response = id %d, %#x, %d bytes; want the internal_exception response", id, excKey, len(resp))
		}
	})

	t.Run("unknown key over the wire", func(t *testing.T) {
		client, peer := newExportDuplex()
		_, cleanup := newExportConnection(t, client)
		defer cleanup()
		if err := writeExportRequest(peer.w, 3, 0xdeadbeef, []byte{1, 2, 3}); err != nil {
			t.Fatalf("writing the unknown request: %v", err)
		}
		id, excKey, resp, err := readExportResponse(peer.r)
		if err != nil {
			t.Fatalf("reading the unknown response: %v", err)
		}
		if id != 3 || excKey != exceptionKey("procedure_not_found") || len(resp) != 0 {
			t.Fatalf("unknown response = id %d, %#x, %d bytes; want procedure_not_found", id, excKey, len(resp))
		}
	})

	t.Run("malformed arguments over the wire", func(t *testing.T) {
		client, peer := newExportDuplex()
		_, cleanup := newExportConnection(t, client)
		defer cleanup()
		if err := writeExportRequest(peer.w, 4, syntax.ProcedureKey("echo"), []byte{0xff}); err != nil {
			t.Fatalf("writing the malformed request: %v", err)
		}
		id, excKey, resp, err := readExportResponse(peer.r)
		if err != nil {
			t.Fatalf("reading the malformed response: %v", err)
		}
		if id != 4 || excKey != exceptionKey("invalid_arguments") || len(resp) != 0 {
			t.Fatalf("malformed response = id %d, %#x, %d bytes; want invalid_arguments", id, excKey, len(resp))
		}
	})

	t.Run("success over the wire", func(t *testing.T) {
		client, peer := newExportDuplex()
		_, cleanup := newExportConnection(t, client)
		defer cleanup()
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "wire hello"))
		if err := writeExportRequest(peer.w, 5, syntax.ProcedureKey("echo"), payload); err != nil {
			t.Fatalf("writing the echo request: %v", err)
		}
		id, excKey, resp, err := readExportResponse(peer.r)
		if err != nil {
			t.Fatalf("reading the echo response: %v", err)
		}
		if id != 5 || excKey != 0 {
			t.Fatalf("echo response = id %d, %#x; want success", id, excKey)
		}
		out, rest, err := exportfixture.Codecs.DecodeString(resp)
		if err != nil || len(rest) != 0 || out != "wire hello" {
			t.Fatalf("echo response = %q, rest %d, err %v", out, len(rest), err)
		}
	})
}

// TestExportRequestDecoders drives every generated request decoder
// directly: a valid payload decodes to the exact parameter values with
// the exact unconsumed remainder, a truncated or malformed payload is
// an error, and trailing bytes are returned as the remainder, which
// the dispatch turns into invalid_arguments.
func TestExportRequestDecoders(t *testing.T) {
	t.Run("echo", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeString(nil, "hi"))
		v, rest, err := exportfixture.Requests.DecodeEcho(payload)
		if err != nil || len(rest) != 0 || v != "hi" {
			t.Fatalf("DecodeEcho = %q, rest %d, err %v", v, len(rest), err)
		}
		if _, _, err := exportfixture.Requests.DecodeEcho(payload[:1]); err == nil {
			t.Fatal("a truncated echo payload was accepted")
		}
		v, rest, err = exportfixture.Requests.DecodeEcho(append(append([]byte{}, payload...), 0x00))
		if err != nil || v != "hi" || len(rest) != 1 {
			t.Fatalf("DecodeEcho with a trailing byte = %q, rest %d, err %v", v, len(rest), err)
		}
	})

	t.Run("add", func(t *testing.T) {
		payload := append(mustVec(exportfixture.Codecs.EncodeInt64(nil, 1)), mustVec(exportfixture.Codecs.EncodeInt64(nil, 2))...)
		a, b, rest, err := exportfixture.Requests.DecodeAdd(payload)
		if err != nil || len(rest) != 0 || a != 1 || b != 2 {
			t.Fatalf("DecodeAdd = %d, %d, rest %d, err %v", a, b, len(rest), err)
		}
		if _, _, _, err := exportfixture.Requests.DecodeAdd(payload[:8]); err == nil {
			t.Fatal("a truncated add payload was accepted")
		}
	})

	t.Run("paint", func(t *testing.T) {
		origin := mustVec(exportfixture.Codecs.EncodePaintOrigin(nil, exportfixture.PaintOrigin{X: 1, Y: 2}))
		size := mustVec(exportfixture.Codecs.EncodePaintSize(nil, exportfixture.PaintSize{Width: 3, Height: 4}))
		payload := append(origin, size...)
		o, s, rest, err := exportfixture.Requests.DecodePaint(payload)
		if err != nil || len(rest) != 0 {
			t.Fatalf("DecodePaint = err %v, rest %d", err, len(rest))
		}
		if o != (exportfixture.PaintOrigin{X: 1, Y: 2}) || s != (exportfixture.PaintSize{Width: 3, Height: 4}) {
			t.Fatalf("DecodePaint = %+v, %+v", o, s)
		}
		if _, _, _, err := exportfixture.Requests.DecodePaint(payload[:len(payload)-1]); err == nil {
			t.Fatal("a truncated paint payload was accepted")
		}
		_, _, rest, err = exportfixture.Requests.DecodePaint(append(append([]byte{}, payload...), 0x00))
		if err != nil || len(rest) != 1 {
			t.Fatalf("DecodePaint with a trailing byte: rest %d, err %v", len(rest), err)
		}
	})

	t.Run("fetch", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeUserID(nil, 9))
		id, rest, err := exportfixture.Requests.DecodeFetch(payload)
		if err != nil || len(rest) != 0 || id != 9 {
			t.Fatalf("DecodeFetch = %d, rest %d, err %v", id, len(rest), err)
		}
	})

	t.Run("wave", func(t *testing.T) {
		payload := mustVec(exportfixture.Codecs.EncodeListUint8(nil, []uint8{1, 2}))
		samples, rest, err := exportfixture.Requests.DecodeWave(payload)
		if err != nil || len(rest) != 0 || len(samples) != 2 || samples[1] != 2 {
			t.Fatalf("DecodeWave = %v, rest %d, err %v", samples, len(rest), err)
		}
		// A count that exceeds the payload is an error.
		if _, _, err := exportfixture.Requests.DecodeWave([]byte{0xff, 0xff}); err == nil {
			t.Fatal("a wave payload with an oversized count was accepted")
		}
	})

	t.Run("sample", func(t *testing.T) {
		data := mustVec(exportfixture.Codecs.EncodeBytes(nil, []byte{1}))
		channel := mustVec(exportfixture.Codecs.EncodeUint8(nil, 5))
		d, c, rest, err := exportfixture.Requests.DecodeSample(append(data, channel...))
		if err != nil || len(rest) != 0 || len(d) != 1 || c != 5 {
			t.Fatalf("DecodeSample = %v, %d, rest %d, err %v", d, c, len(rest), err)
		}
	})

	t.Run("ping and crash", func(t *testing.T) {
		rest, err := exportfixture.Requests.DecodePing(nil)
		if err != nil || len(rest) != 0 {
			t.Fatalf("DecodePing(empty) = rest %d, err %v", len(rest), err)
		}
		rest, err = exportfixture.Requests.DecodePing([]byte{1})
		if err != nil || len(rest) != 1 {
			t.Fatalf("DecodePing(1 byte) = rest %d, err %v; the payload must be returned as the remainder", len(rest), err)
		}
		mode, rest, err := exportfixture.Requests.DecodeCrash(mustVec(exportfixture.Codecs.EncodeString(nil, "panic")))
		if err != nil || len(rest) != 0 || mode != "panic" {
			t.Fatalf("DecodeCrash = %q, rest %d, err %v", mode, len(rest), err)
		}
	})
}
