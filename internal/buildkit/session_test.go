package buildkit

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
)

// A build's session ends as its client hangs up, not seconds later when
// its health checks have failed: gRPC cannot dial the session's one
// connection again.
func TestSessionEndsWithItsConnection(t *testing.T) {
	sm, err := session.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const id = "e2e-session"
	done := make(chan error, 1)
	go func() {
		done <- sm.HandleConn(context.Background(), server, map[string][]string{"X-Docker-Expose-Session-Uuid": {id}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sm.Get(ctx, id, false); err != nil {
		t.Fatalf("the session did not start: %v", err)
	}
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the session outlived its connection")
	}
}
