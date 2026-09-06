package intercall

import (
	"context"
	"sync"
)

// Connection is the opaque runtime handle for one InterCall connection.
//
// A connection owns its byte stream and exactly one export and one import
// binding, neither of which can change after construction. It has two
// lifecycle conditions, active and terminal: terminal selection fixes one
// permanent cause, closes the stream exactly once, and cancels handler
// contexts. A nonnil *Connection is bound into contexts with
// WithConnection and retrieved with ConnectionFromContext; the nil
// *Connection zero value is invalid. Construction and the call and shutdown
// methods are part of the generated-code SPI.
type Connection struct {
	// Connection state, guarded by mu: terminal selection, outgoing request
	// ID allocation, and the pending-call map.
	mu       sync.Mutex
	terminal chan struct{} // closed exactly once when the first cause is selected
	cause    error         // permanent terminal cause; non-nil once terminal is closed

	// Outgoing-call state, guarded by mu. nextID is the next monotonic
	// 63-bit request ID, allocated from 0 through 0x7fffffffffffffff and
	// never reused, including after completion or local cancellation.
	// pending holds one entry per admitted outgoing request that is still
	// eligible for its single outcome; presence means the request is
	// eligible, and removal transfers exclusive ownership to exactly one
	// response, per-call cancellation, or terminal teardown.
	nextID  uint64
	pending map[uint64]*pendingCall

	// Incoming-call state, guarded by mu. incoming holds one entry per
	// incoming request ID from ordered admission until its response write
	// completes: a fresh request reserves its ID before its handler starts,
	// the handler marks the ID as writing at write admission, and a
	// successful write releases the ID or hands it to the one deferred next
	// generation. Reuse of an ID before the prior response enters write
	// admission is a terminal protocol error; reuse during that write is
	// fully buffered as one deferred generation; reuse after the successful
	// write is allowed. Terminal publication drains every entry, including
	// a deferred generation, so teardown leaves no incoming state behind.
	// Incoming and outgoing ID spaces are independent.
	incoming map[uint64]*incomingCall

	// gate is the connection-wide write gate: requests and responses share
	// it, exactly one frame write proceeds at a time, and frames never
	// interleave. Acquiring the gate waits on terminal selection and, for
	// outgoing calls, the call context's Done without holding mu; when both
	// are held the order is gate first, then mu. mu is never held while
	// waiting for the gate or while calling stream Write or Close.
	gate writeGate

	// Immutable ownership state, fixed at construction.
	stream ByteStream
	export ExportBinding
	imp    ImportBinding

	// Context state, fixed at construction.
	ctx            context.Context // construction context watched by the observer
	connCtx        context.Context // parent of handler contexts
	cancelHandlers context.CancelFunc

	// Handler lifetime state. Every handler launched by the receive loop is
	// counted before its goroutine starts and releases one count only after
	// its complete dispatch, response, write, or terminal-discard path. A
	// deferred same-ID generation is added while its parent still holds a
	// count, so WaitForHandlers can wait after receiveExit without racing a
	// valid launch.
	handlers sync.WaitGroup

	// Lifecycle completion channels. observerExit closes when the context
	// observer goroutine exits, receiveExit when the sole receive-loop
	// goroutine exits, and teardown when the asynchronous cleanup owner
	// finishes teardown: every unclaimed pending call completed, the stream
	// closed exactly once, and handler contexts canceled.
	observerExit chan struct{}
	receiveExit  chan struct{}
	teardown     chan struct{}
}

// newConnection is the construction core behind the public connection
// constructor. It validates its arguments before taking ownership of the
// stream: a nil context or stream interface, a zero export or import
// binding, and an already available ctx.Err() are all rejected before the
// stream is claimed. On success it stores exactly one export and one import
// handle, derives the connection context, and starts the sole
// context-observer goroutine. The public NewConnection additionally starts
// the sole receive-loop goroutine after this core returns.
func newConnection(ctx context.Context, stream ByteStream, export ExportBinding, imp ImportBinding) (*Connection, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	if stream == nil {
		return nil, ErrInvalidArgument
	}
	if export == (ExportBinding{}) {
		return nil, ErrInvalidArgument
	}
	if imp == (ImportBinding{}) {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	connCtx, cancel := context.WithCancel(ctx)
	c := &Connection{
		terminal:       make(chan struct{}),
		pending:        make(map[uint64]*pendingCall),
		incoming:       make(map[uint64]*incomingCall),
		gate:           newWriteGate(),
		stream:         stream,
		export:         export,
		imp:            imp,
		ctx:            ctx,
		connCtx:        connCtx,
		cancelHandlers: cancel,
		observerExit:   make(chan struct{}),
		receiveExit:    make(chan struct{}),
		teardown:       make(chan struct{}),
	}
	go c.observeContext()
	return c, nil
}

// NewConnection validates and constructs a connection, taking ownership of
// the stream: a nil context or stream interface, a zero export or import
// binding, and an already available ctx.Err() are all rejected before the
// stream is claimed. On success the connection stores exactly one export
// and one import handle — neither can change — and is fully initialized: the
// context-observer goroutine and the sole receive-loop goroutine both start
// before NewConnection returns, so a call is either on an active connection
// or returns its terminal cause. There is no generated Run, startup state,
// or startup wait.
func NewConnection(ctx context.Context, stream ByteStream, export ExportBinding, imp ImportBinding) (*Connection, error) {
	c, err := newConnection(ctx, stream, export, imp)
	if err != nil {
		return nil, err
	}
	go c.receiveLoop()
	return c, nil
}

// Close terminates the connection by selecting ErrClosed if no permanent
// cause is selected yet and otherwise does nothing. In either case terminal
// publication completes under the state lock before Close returns. Close
// never waits for the receive loop, the context observer, handler
// goroutines, blocked gate waiters, or stream cleanup; Wait reports the
// permanent terminal cause after teardown completes.
func (c *Connection) Close() error {
	if c == nil {
		return ErrInvalidArgument
	}
	c.selectTerminal(ErrClosed)
	return nil
}

// Wait blocks until the sole receive loop exits, terminal teardown and
// stream cleanup complete, and the context observer exits, then returns the
// permanent terminal cause; it never returns nil. Wait does not wait for
// handler goroutines: handlers that ignore cancellation may outlive it, but
// terminal state prevents them from beginning a later response write.
func (c *Connection) Wait() error {
	if c == nil {
		return ErrInvalidArgument
	}
	<-c.teardown
	<-c.receiveExit
	<-c.observerExit
	c.mu.Lock()
	cause := c.cause
	c.mu.Unlock()
	return cause
}

// WaitForHandlers is the owner-side lifecycle barrier. It first waits for the
// existing connection teardown, receive loop, and context observer through
// Wait, then waits for every request handler admitted by the connection. The
// handler count covers dispatch, response construction, write admission and
// write, or terminal discard, including deferred same-ID generations. It has
// no timeout or forced cancellation and may therefore block indefinitely when
// a handler ignores cancellation. Do not call it from an active handler or
// stream cleanup; Close and Wait intentionally do not call it.
func (c *Connection) WaitForHandlers() error {
	if c == nil {
		return ErrInvalidArgument
	}
	err := c.Wait()
	c.handlers.Wait()
	return err
}

// selectTerminal attempts to select err as the permanent terminal cause. All
// terminal events — explicit close, read or write failure, EOF or half-close,
// protocol error, and context cancellation — use this same lock-protected
// selection, and err must be non-nil. The first selector wins: publication
// under the state lock fixes the cause, closes the terminal channel, and
// transfers every unclaimed pending entry away from later response or
// per-call-cancellation claims. The winner then starts exactly one
// asynchronous cleanup owner; losers return immediately without attempting
// teardown.
func (c *Connection) selectTerminal(err error) {
	c.mu.Lock()
	if c.cause != nil {
		c.mu.Unlock()
		return
	}
	c.cause = err
	close(c.terminal)
	// Terminal publication claims every unclaimed pending call under the
	// same lock: removal transfers exclusive ownership to terminal
	// selection before any cleanup step runs, so neither a response nor
	// per-call cancellation can claim a transferred entry after Close
	// returns. Entries already claimed by a response or per-call
	// cancellation are absent and are completed by their owners.
	claims := make([]*pendingCall, 0, len(c.pending))
	for id, pc := range c.pending {
		delete(c.pending, id)
		claims = append(claims, pc)
	}
	// Terminal publication also drains every incoming and deferred request
	// entry under the same lock: a buffered or deferred request is never
	// dispatched after terminal wins, and teardown leaves no incoming state
	// behind. Handlers already admitted abandon their response at write
	// admission; a write already in flight is unblocked by stream closure,
	// and its completion discards the deferred generation under terminal
	// state.
	clear(c.incoming)
	c.mu.Unlock()

	// Teardown, exactly once, by the one asynchronous cleanup owner the
	// winner started, outside the lock. Pending calls complete first so
	// waiting callers wake without waiting for the stream cleanup; the
	// cleanup error is suppressed. teardown closes only after every
	// teardown step, so Wait observes complete terminal teardown and stream
	// cleanup before returning, while Close returns after publication
	// without waiting for a blocked gate, Write, or ByteStream.Close.
	go c.runTeardown(err, claims)
}

// terminalCause returns the permanent terminal cause. It is read under the
// connection lock; after the terminal channel has closed the value is
// already visible through the channel close, and the lock keeps every other
// read consistent.
func (c *Connection) terminalCause() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// runTeardown is the one asynchronous cleanup owner for a terminal
// connection: it delivers the permanent cause to every transferred pending
// call, closes the owned stream exactly once, cancels handler contexts, and
// then closes teardown. It runs in exactly one goroutine per connection —
// the winner of terminal selection — so the stream closes exactly once and
// Wait observes complete cleanup. A stream cleanup error never replaces or
// joins the published cause.
func (c *Connection) runTeardown(err error, claims []*pendingCall) {
	for _, pc := range claims {
		pc.complete(err)
	}
	_ = c.stream.Close()
	c.cancelHandlers()
	close(c.teardown)
}

// observeContext is the context-observer goroutine started by construction.
// It waits for either the construction context's Done channel or the
// terminal-selection channel. Context cancellation attempts terminal
// selection with the exact ctx.Err() value; the runtime never uses
// context.Cause or wraps that value. A nil Done channel, as from
// context.Background, simply disables the context case. When another event
// wins selection, the observer rechecks terminal state under the same lock
// and exits without attempting a new cause; when context cancellation wins,
// the observer completes selection — publication and the start of the one
// asynchronous cleanup owner — before exiting.
func (c *Connection) observeContext() {
	defer close(c.observerExit)

	done := c.ctx.Done()
	if done == nil {
		<-c.terminal
		return
	}
	select {
	case <-done:
		c.selectTerminal(c.ctx.Err())
	case <-c.terminal:
	}
}

// newHandlerContext derives one bound, per-handler context from the
// connection context. The connection cancels every derived context at
// terminal selection; the returned cancel func completes an individual
// handler. The handler context carries the connection through
// WithConnection.
func (c *Connection) newHandlerContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.connCtx)
	return WithConnection(ctx, c), cancel
}

// checkImport verifies that a caller's import handle is exactly the
// connection's stored import binding. A zero or different handle returns
// ErrBindingMismatch. Argument and binding validation happens before any
// terminal-state inspection.
func (c *Connection) checkImport(imp ImportBinding) error {
	if imp != c.imp {
		return ErrBindingMismatch
	}
	return nil
}
