package intercall

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	barrierProcedureNotFoundKey = uint64(0x970e76fcc5e2dacb)
	barrierInvalidArgumentsKey  = uint64(0x3f5fc972f8477b07)
)

// registerHandlerBarrierCleanup releases every test-controlled handler or
// stream blocker before joining the connection. In particular, this cleanup
// remains safe when an assertion above calls t.Fatal before a waiter joins.
func registerHandlerBarrierCleanup(t *testing.T, c *Connection, release func(), closeInput func()) {
	t.Helper()
	t.Cleanup(func() {
		release()
		closeInput()
		_ = c.Close()

		done := make(chan error, 1)
		go func() { done <- c.WaitForHandlers() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("cleanup timed out joining connection handlers")
		}
	})
}

func barrierWait(t *testing.T, c *Connection) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.WaitForHandlers() }()
	return done
}

func requireBarrierResult(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("WaitForHandlers() = %v, want exactly %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WaitForHandlers")
	}
}

func requireBarrierBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("WaitForHandlers returned %v before the handler was released", err)
	case <-time.After(25 * time.Millisecond):
	}
}

func requireWaitResult(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("Wait() = %v, want exactly %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Wait")
	}
}

func waitDeferredIncoming(t *testing.T, c *Connection, id uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		ic := c.incoming[id]
		ready := ic != nil && ic.writing && ic.hasDeferred
		c.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the deferred same-ID request")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWaitForHandlersJoinsPausedDispatch(t *testing.T) {
	stream := newPipeStream()
	dispatchEntered := make(chan struct{})
	dispatchRelease := make(chan struct{})
	var enteredOnce, releaseOnce, inputCloseOnce sync.Once

	c := newReceiveTestConn(t, stream, func(context.Context, uint64, []byte) (uint64, []byte) {
		enteredOnce.Do(func() { close(dispatchEntered) })
		<-dispatchRelease
		return 0, nil
	})
	registerHandlerBarrierCleanup(t, c,
		func() { releaseOnce.Do(func() { close(dispatchRelease) }) },
		func() { inputCloseOnce.Do(stream.closeWriter) },
	)

	stream.feed(t, buildFrame(requestFrame, 7, 0x42, nil))
	select {
	case <-dispatchEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reached dispatch")
	}

	// Close remains prompt and the legacy Wait observes teardown without
	// waiting for the handler that is deliberately ignoring cancellation.
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the paused handler")
	}

	oldWait := make(chan error, 1)
	go func() { oldWait <- c.Wait() }()
	requireWaitResult(t, oldWait, ErrClosed)
	// Wait has joined the receive loop while the admitted handler is still
	// paused. This is the initial Add-before-launch/read-exit boundary: the
	// handler count must already be nonzero when the owner starts its barrier.
	select {
	case <-c.receiveExit:
	default:
		t.Fatal("Wait returned before the receive loop exited")
	}

	barrier := barrierWait(t, c)
	requireBarrierBlocked(t, barrier)

	releaseOnce.Do(func() { close(dispatchRelease) })
	requireBarrierResult(t, barrier, ErrClosed)
}

func TestWaitForHandlersJoinsConcurrentHandlers(t *testing.T) {
	stream := newPipeStream()
	entered := make(chan uint64, 2)
	release := make(chan struct{})
	var releaseOnce, inputCloseOnce sync.Once

	c := newReceiveTestConn(t, stream, func(_ context.Context, key uint64, _ []byte) (uint64, []byte) {
		entered <- key
		<-release
		return 0, nil
	})
	registerHandlerBarrierCleanup(t, c,
		func() { releaseOnce.Do(func() { close(release) }) },
		func() { inputCloseOnce.Do(stream.closeWriter) },
	)

	stream.feed(t, buildFrame(requestFrame, 1, 0x41, nil))
	stream.feed(t, buildFrame(requestFrame, 2, 0x42, nil))
	seen := map[uint64]bool{}
	for range 2 {
		select {
		case key := <-entered:
			seen[key] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent handlers")
		}
	}
	if !seen[0x41] || !seen[0x42] {
		t.Fatalf("handler keys = %v, want both 0x41 and 0x42", seen)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	oldWait := make(chan error, 1)
	go func() { oldWait <- c.Wait() }()
	requireWaitResult(t, oldWait, ErrClosed)

	barrier := barrierWait(t, c)
	requireBarrierBlocked(t, barrier)
	releaseOnce.Do(func() { close(release) })
	requireBarrierResult(t, barrier, ErrClosed)
}

func TestWaitForHandlersCountsDeferredGenerationBeforeParentDone(t *testing.T) {
	stream := newPipeStream()
	firstWriteEntered := make(chan struct{})
	firstWriteRelease := make(chan struct{})
	deferredEntered := make(chan struct{})
	deferredRelease := make(chan struct{})
	var writeOnce, firstReleaseOnce, deferredOnce, deferredReleaseOnce, inputCloseOnce sync.Once
	var dispatches atomic.Int32

	var writes atomic.Int32
	stream.write = func(p []byte) (int, error) {
		if writes.Add(1) == 1 {
			writeOnce.Do(func() { close(firstWriteEntered) })
			<-firstWriteRelease
		}
		return len(p), nil
	}

	c := newReceiveTestConn(t, stream, func(context.Context, uint64, []byte) (uint64, []byte) {
		if dispatches.Add(1) == 2 {
			deferredOnce.Do(func() { close(deferredEntered) })
			<-deferredRelease
		}
		return 0, nil
	})
	registerHandlerBarrierCleanup(t, c,
		func() {
			firstReleaseOnce.Do(func() { close(firstWriteRelease) })
			deferredReleaseOnce.Do(func() { close(deferredRelease) })
		},
		func() { inputCloseOnce.Do(stream.closeWriter) },
	)

	stream.feed(t, buildFrame(requestFrame, 9, 0x42, nil))
	select {
	case <-firstWriteEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first response write never started")
	}

	// The receive loop remains live while the first response is writing, so
	// this duplicate is reserved as the one deferred generation.
	stream.feed(t, buildFrame(requestFrame, 9, 0x42, nil))
	waitDeferredIncoming(t, c, 9)

	firstReleaseOnce.Do(func() { close(firstWriteRelease) })
	select {
	case <-deferredEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred handler never launched")
	}
	if got := dispatches.Load(); got != 2 {
		t.Fatalf("dispatch count = %d, want parent and deferred handlers", got)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	oldWait := make(chan error, 1)
	go func() { oldWait <- c.Wait() }()
	requireWaitResult(t, oldWait, ErrClosed)

	barrier := barrierWait(t, c)
	requireBarrierBlocked(t, barrier)

	deferredReleaseOnce.Do(func() { close(deferredRelease) })
	requireBarrierResult(t, barrier, ErrClosed)
}

func TestWaitForHandlersTracksZeroMalformedUnknownAndPanic(t *testing.T) {
	cases := []struct {
		name        string
		requestKey  uint64
		responseKey uint64
		panic       bool
	}{
		{name: "zero success", requestKey: 1, responseKey: 0},
		{name: "malformed arguments", requestKey: 2, responseKey: barrierInvalidArgumentsKey},
		{name: "unknown procedure", requestKey: 3, responseKey: barrierProcedureNotFoundKey},
		{name: "dispatch panic", requestKey: 4, responseKey: internalExceptionKey, panic: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := newPipeStream()
			entered := make(chan struct{})
			release := make(chan struct{})
			var enteredOnce, releaseOnce, inputCloseOnce sync.Once

			c := newReceiveTestConn(t, stream, func(context.Context, uint64, []byte) (uint64, []byte) {
				enteredOnce.Do(func() { close(entered) })
				<-release
				if tc.panic {
					panic("test dispatch panic")
				}
				return tc.responseKey, nil
			})
			registerHandlerBarrierCleanup(t, c,
				func() { releaseOnce.Do(func() { close(release) }) },
				func() { inputCloseOnce.Do(stream.closeWriter) },
			)

			stream.feed(t, buildFrame(requestFrame, tc.requestKey, tc.requestKey, nil))
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("handler never reached dispatch")
			}

			if err := c.Close(); err != nil {
				t.Fatalf("Close() = %v, want nil", err)
			}
			oldWait := make(chan error, 1)
			go func() { oldWait <- c.Wait() }()
			requireWaitResult(t, oldWait, ErrClosed)

			barrier := barrierWait(t, c)
			requireBarrierBlocked(t, barrier)
			releaseOnce.Do(func() { close(release) })
			requireBarrierResult(t, barrier, ErrClosed)
		})
	}
}

func TestWaitForHandlersCancellationAndMultipleWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newPipeStream()
	dispatchEntered := make(chan struct{})
	dispatchRelease := make(chan struct{})
	var enteredOnce, releaseOnce, inputCloseOnce sync.Once

	export, err := NewExportBinding(func(context.Context, uint64, []byte) (uint64, []byte) {
		enteredOnce.Do(func() { close(dispatchEntered) })
		<-dispatchRelease
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewConnection(ctx, stream, export, NewImportBinding())
	if err != nil {
		t.Fatal(err)
	}
	registerHandlerBarrierCleanup(t, c,
		func() { releaseOnce.Do(func() { close(dispatchRelease) }) },
		func() { inputCloseOnce.Do(stream.closeWriter) },
	)

	stream.feed(t, buildFrame(requestFrame, 5, 0x42, nil))
	select {
	case <-dispatchEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reached dispatch")
	}

	cancel()
	waitTerminal(t, c)

	const waiters = 4
	done := make([]<-chan error, waiters)
	for i := range done {
		done[i] = barrierWait(t, c)
	}
	for _, waiter := range done {
		requireBarrierBlocked(t, waiter)
	}

	releaseOnce.Do(func() { close(dispatchRelease) })
	for i, waiter := range done {
		select {
		case got := <-waiter:
			if got != context.Canceled {
				t.Errorf("waiter %d: WaitForHandlers() = %v, want context.Canceled", i, got)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("waiter %d: timed out waiting for WaitForHandlers", i)
		}
	}
}

func TestWaitForHandlersJoinsPartialResponseAfterTerminal(t *testing.T) {
	stream := newPipeStream()
	partialWriteEntered := make(chan struct{})
	partialWriteRelease := make(chan struct{})
	var writes atomic.Int32
	var partialOnce, releaseOnce, inputCloseOnce sync.Once

	stream.write = func(p []byte) (int, error) {
		switch writes.Add(1) {
		case 1:
			return 1, nil
		case 2:
			partialOnce.Do(func() { close(partialWriteEntered) })
			<-partialWriteRelease
		}
		return len(p), nil
	}
	c := newReceiveTestConn(t, stream, func(context.Context, uint64, []byte) (uint64, []byte) {
		return 0, []byte("response")
	})
	registerHandlerBarrierCleanup(t, c,
		func() { releaseOnce.Do(func() { close(partialWriteRelease) }) },
		func() { inputCloseOnce.Do(stream.closeWriter) },
	)

	stream.feed(t, buildFrame(requestFrame, 12, 0x42, nil))
	select {
	case <-partialWriteEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("partial response write never stalled")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	oldWait := make(chan error, 1)
	go func() { oldWait <- c.Wait() }()
	requireWaitResult(t, oldWait, ErrClosed)

	barrier := barrierWait(t, c)
	requireBarrierBlocked(t, barrier)
	releaseOnce.Do(func() { close(partialWriteRelease) })
	requireBarrierResult(t, barrier, ErrClosed)
}

func TestWaitForHandlersNilReceiver(t *testing.T) {
	var c *Connection
	if err := c.WaitForHandlers(); err != ErrInvalidArgument {
		t.Fatalf("WaitForHandlers() on nil receiver = %v, want ErrInvalidArgument", err)
	}
}
