package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cerasos/intercall/go"
	"github.com/cerasos/intercall/go/internal/integration/fixtures/e2eimport"
	"github.com/cerasos/intercall/go/internal/integration/fixtures/provider"
)

// barrierWriteStream retains one generated response write after the generated
// export dispatch has decoded the request and encoded its return. Its only
// purpose is to make the native handler barrier observable at the final
// response-write boundary without changing the runtime or generator.
type barrierWriteStream struct {
	intercall.ByteStream
	writeEntered chan struct{}
	writeRelease chan struct{}
	enteredOnce  sync.Once
	releaseOnce  sync.Once
}

func (s *barrierWriteStream) Write(p []byte) (int, error) {
	s.enteredOnce.Do(func() { close(s.writeEntered) })
	<-s.writeRelease
	return s.ByteStream.Write(p)
}

func (s *barrierWriteStream) releaseWrite() {
	s.releaseOnce.Do(func() { close(s.writeRelease) })
}

func TestGeneratedHandlerBarrierCoversDecodeReturnEncodeAndWrite(t *testing.T) {
	const waitID = uint32(0x7f00cafe)

	clientStream, serverBase := newDuplex()
	serverStream := &barrierWriteStream{
		ByteStream:   serverBase,
		writeEntered: make(chan struct{}),
		writeRelease: make(chan struct{}),
	}
	client := newConnection(t, context.Background(), clientStream)
	server := newConnection(t, context.Background(), serverStream)
	t.Cleanup(func() {
		// Cleanup releases both test-controlled boundaries before joining
		// either connection, including when an assertion above calls Fatal.
		provider.ReleaseWait(waitID)
		serverStream.releaseWrite()
		_ = server.Close()
		_ = client.Close()
		_ = server.WaitForHandlers()
		_ = client.Wait()
	})

	callDone := make(chan error, 1)
	go func() {
		_, err := e2eimport.Wait(bind(client), waitID)
		callDone <- err
	}()
	eventually(t, "generated wait handler to decode its argument", func() bool {
		return provider.IsWaiting(waitID)
	})

	// IsWaiting is entered by the generated provider only after generated
	// argument decoding. Release the provider, then wait for its generated
	// return value to be encoded and for the runtime handler to enter Write.
	provider.ReleaseWait(waitID)
	select {
	case <-serverStream.writeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("generated handler did not reach the response write after return encoding")
	}

	if err := server.Close(); err != nil {
		t.Fatalf("server Close() = %v, want nil", err)
	}
	if err := server.Wait(); err != intercall.ErrClosed {
		t.Fatalf("server Wait() = %v, want ErrClosed", err)
	}

	barrierDone := make(chan error, 1)
	go func() { barrierDone <- server.WaitForHandlers() }()
	select {
	case err := <-barrierDone:
		t.Fatalf("WaitForHandlers returned while generated response Write was blocked: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	serverStream.releaseWrite()
	select {
	case err := <-barrierDone:
		if err != intercall.ErrClosed {
			t.Fatalf("WaitForHandlers() = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForHandlers did not join the generated handler")
	}

	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("generated caller returned nil after its peer closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("generated caller did not unwind after response-write teardown")
	}
}
