package intercall

import (
	"context"
	"fmt"
)

// internalExceptionKey is the FNV-0 key of the fixed no-payload wire
// exception "internal_exception" (SPEC.md Fixed Go Runtime Exceptions). The
// runtime's one recovery around the complete dispatch selects it when a
// dispatch or provider panic escapes. The generated static procedure switch
// selects procedure_not_found and invalid_arguments itself.
const internalExceptionKey = 0x1aaec22e85996f50

// receiveLoop is the sole reader of the connection's stream, started by
// NewConnection before it returns. It reads one complete frame at a time
// with full-read semantics: each 24-byte header, then the complete payload
// in a fresh owned buffer after the wire length has been checked against
// the maximum accepted frame payload of exactly 64 MiB. A response is
// completely buffered before lookup: a pending ID is claimed and decoded in
// this goroutine, while an unmatched ID stays opaque and its payload is
// never validated. Request admission is one ordered decision under the
// connection lock: terminal selection wins over a buffered request, so a
// frame is never dispatched after terminal has won; a fresh ID transfers
// its complete payload to one new, unbounded handler goroutine; a duplicate
// observed before the prior response enters write admission is a terminal
// protocol error; and a duplicate observed during that write is fully
// buffered as the one deferred next generation without parking this loop,
// while a further same-ID request cannot also queue. Every read failure is
// terminal: an incomplete header or payload is a transport failure, and an
// over-ceiling wire length or structural frame failure is a protocol error;
// readFrame classifies and prefixes both.
func (c *Connection) receiveLoop() {
	defer close(c.receiveExit)
	for {
		hdr, payload, err := readFrame(c.stream)
		if err != nil {
			c.selectTerminal(err)
			return
		}
		switch hdr.kind {
		case responseFrame:
			c.claimResponse(hdr.requestID, hdr.key, payload)
		case requestFrame:
			switch c.admitIncoming(hdr.requestID, hdr.key, payload) {
			case admissionDiscard:
				// Terminal selection already won: the buffered request is
				// never dispatched, and the loop stops processing.
				return
			case admissionDeferred:
				// Fully buffered as the one deferred next generation; the
				// loop continues without parking.
			case admissionTerminal:
				c.selectTerminal(fmt.Errorf("intercall: incoming request ID %d already active: %w", hdr.requestID, ErrProtocol))
				return
			case admissionDispatch:
				c.startHandler(hdr.requestID, hdr.key, payload)
			}
		}
	}
}

// incomingAdmission classifies one buffered incoming request frame under
// the connection lock, making terminal selection and request admission one
// ordered decision.
//
//   - admissionDiscard reports that terminal selection already won: the
//     buffered request is dropped and never dispatched.
//   - admissionDispatch reports a fresh request ID: the caller starts one
//     handler goroutine with the transferred owned payload.
//   - admissionDeferred reports a duplicate observed while the prior
//     response write is active: the payload is fully buffered as the one
//     deferred next generation, and a further same-ID request cannot queue.
//   - admissionTerminal reports a duplicate observed before the prior
//     response enters write admission (or beyond the one deferred
//     generation): reuse is a terminal protocol error.
type incomingAdmission uint8

const (
	admissionDiscard incomingAdmission = iota
	admissionDispatch
	admissionDeferred
	admissionTerminal
)

// incomingCall is the per-ID incoming-request state, guarded by the
// connection lock. Presence in the incoming map means the ID is reserved:
// a request of that ID is active or one generation is deferred. writing
// reports that the current generation's response write has entered write
// admission (the gate is held and the terminal recheck passed), so a
// duplicate of the ID is deferred rather than terminal. hasDeferred holds
// at most one fully buffered duplicate observed while that write is active:
// the deferred next generation is admitted by a successful write and
// discarded by a write failure or terminal selection. Incoming and outgoing
// ID spaces are independent, and reuse after release is allowed.
type incomingCall struct {
	writing     bool
	hasDeferred bool
	deferredKey uint64

	// deferredPayload is the complete owned payload of the deferred
	// duplicate, transferred from the receive loop's frame buffer and
	// retained until the prior response write completes.
	deferredPayload []byte
}

// deferredRequest is one fully buffered duplicate request reserved as the
// next generation while the prior response write is active. A successful
// write returns it for admission; a write failure or terminal selection
// discards it.
type deferredRequest struct {
	key     uint64
	payload []byte
}

// admitIncoming makes the one ordered admission decision for a buffered
// incoming request frame under the connection lock. Terminal selection
// wins over every buffered request, so after terminal has won a frame is
// never dispatched. A fresh ID is reserved for one handler goroutine. A
// duplicate observed before the current generation's response enters write
// admission — its handler is still dispatching, waiting for the gate, or
// has not passed the admission recheck — is a terminal protocol error,
// reported as admissionTerminal for the caller to select. A duplicate
// observed while that response write is active is fully buffered as the
// one deferred next generation: the receive loop never parks, and a
// further same-ID request cannot also queue.
func (c *Connection) admitIncoming(id, key uint64, payload []byte) incomingAdmission {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cause != nil {
		return admissionDiscard
	}
	if ic, ok := c.incoming[id]; !ok {
		c.incoming[id] = &incomingCall{}
		return admissionDispatch
	} else if ic.writing {
		if ic.hasDeferred {
			return admissionTerminal
		}
		ic.hasDeferred = true
		ic.deferredKey = key
		ic.deferredPayload = payload
		return admissionDeferred
	}
	return admissionTerminal
}

// markIncomingWriting records under the connection lock that this
// handler's response write has entered write admission: the gate is held
// and the terminal recheck passed, so from this point a duplicate of the
// request ID is deferred rather than terminal. The handler calls it exactly
// once, between write admission and the stream write. The entry is present
// unless terminal teardown already drained it, which the admission recheck
// just ruled out.
func (c *Connection) markIncomingWriting(id uint64) {
	if ic := c.incoming[id]; ic != nil {
		ic.writing = true
	}
}

// completeIncomingWrite settles the current generation after its complete
// response write succeeded, under the connection lock. Without a deferred
// generation the ID is released and becomes reusable. With one, the
// deferred request is removed and returned as the admitted next generation
// while the ID stays reserved; the caller starts its handler after
// releasing the gate. A nil return discards the deferred request: the
// entry is absent when terminal selection already drained it, so a write
// failure or terminal selection never admits a deferred generation.
func (c *Connection) completeIncomingWrite(id uint64) *deferredRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	ic, ok := c.incoming[id]
	if !ok {
		return nil
	}
	ic.writing = false
	if !ic.hasDeferred {
		delete(c.incoming, id)
		return nil
	}
	ic.hasDeferred = false
	deferred := &deferredRequest{key: ic.deferredKey, payload: ic.deferredPayload}
	ic.deferredKey = 0
	ic.deferredPayload = nil
	return deferred
}

// startHandler counts one admitted request before launching its unbounded
// handler goroutine. The count is released by the goroutine only after
// handleRequest has completed its entire dispatch, response, write, or
// terminal-discard path. Callers must invoke this before every handler launch;
// in particular, a deferred same-ID generation is started here while its
// parent handler still holds its count.
func (c *Connection) startHandler(id, key uint64, payload []byte) {
	c.handlers.Add(1)
	go func() {
		defer c.handlers.Done()
		c.handleRequest(id, key, payload)
	}()
}

// handleRequest executes one incoming call in its own unbounded handler
// goroutine. The receive loop admitted the request ID before starting it and
// transferred the complete owned payload, which the runtime never reuses.
// The handler derives one bound per-handler context, invokes the export
// dispatch under the single runtime recovery that maps escaped panics to
// internal_exception, fully encodes the selected response, and writes it
// through the shared write gate. The incoming ID remains reserved until the
// complete response write succeeds: write admission marks the ID as writing
// so a concurrent duplicate is deferred rather than terminal, and a
// successful write releases the ID or admits the one deferred next
// generation, while a write failure or terminal selection discards it under
// terminal state. The handler context is canceled when the handler finishes
// or the connection terminates. Terminal state prevents a handler from
// beginning a later response write: a handler that observes terminal
// selection at write admission abandons its response, and a handler already
// writing is unblocked by stream closure.
func (c *Connection) handleRequest(id, key uint64, payload []byte) {
	hctx, hcancel := c.newHandlerContext()
	defer hcancel()

	ekey, epayload := c.invokeDispatch(hctx, key, payload)
	frame := buildFrame(responseFrame, id, ekey, epayload)

	// Write admission: wait for the connection-wide write gate without
	// holding the connection state lock, observing terminal selection so a
	// handler waiting for the gate abandons its response after terminal
	// selection. Once the gate is held, recheck terminal state under the
	// connection lock: if terminal selection won while the handler waited,
	// the response is abandoned and the gate released. Otherwise the
	// admission is one lock-protected action — the terminal recheck plus
	// marking the ID as writing — so a duplicate observed from this point
	// is deferred, not terminal. The lock order is gate first, then mu.
	if err := c.acquireWriteGate(nil); err != nil {
		return
	}
	c.mu.Lock()
	if c.cause != nil {
		c.mu.Unlock()
		c.releaseWriteGate()
		return
	}
	c.markIncomingWriting(id)
	c.mu.Unlock()
	err := writeFull(c.stream, frame)
	c.releaseWriteGate()
	if err != nil {
		c.selectTerminal(err)
		return
	}

	// A successful write settles the current generation and admits the
	// deferred next generation, if one was reserved, even when it arrived
	// before the final local Write returned; the new handler is started
	// after the gate is released so it can contend for it normally.
	if deferred := c.completeIncomingWrite(id); deferred != nil {
		// The current handler still owns its handler count here. Add the
		// deferred generation before launching it so a zero-counter wait
		// cannot race this valid same-ID reuse.
		c.startHandler(id, deferred.key, deferred.payload)
	}
}

// invokeDispatch runs the connection's export dispatch with the handler
// context, the incoming procedure key, and the complete owned request
// payload. One recovery around the complete dispatch maps every escaped
// panic to the internal_exception response; the generated static procedure
// switch selects procedure_not_found and invalid_arguments itself before
// invoking a provider.
func (c *Connection) invokeDispatch(ctx context.Context, key uint64, payload []byte) (ekey uint64, epayload []byte) {
	defer func() {
		if recover() != nil {
			ekey = internalExceptionKey
			epayload = nil
		}
	}()
	return c.export.state.dispatch(ctx, key, payload)
}
