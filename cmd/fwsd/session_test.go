package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonistiigi/fsutil"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/session/filesync"
)

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// A BuildKit session a client attaches through POST /session stays usable:
// BuildKit's calls into it, its health checks among them, reach the client.
func TestSessionStaysAttached(t *testing.T) {
	for _, debug := range []bool{false, true} {
		sm, err := session.NewManager()
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
			if err := sm.HandleHTTPRequest(r.Context(), w, r); err != nil {
				t.Error(err)
			}
		})
		sock := filepath.Join(t.TempDir(), "s")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := newHTTPServer(mux, debug)
		go srv.Serve(l)
		ctx, cancel := context.WithCancel(context.Background())
		s, err := session.NewSession(ctx, "key")
		if err != nil {
			t.Fatal(err)
		}
		src := t.TempDir()
		big := make([]byte, 64<<20)
		rand.Read(big)
		if err := os.WriteFile(filepath.Join(src, "big"), big, 0o644); err != nil {
			t.Fatal(err)
		}
		fsys, err := fsutil.NewFS(src)
		if err != nil {
			t.Fatal(err)
		}
		s.Allow(filesync.NewFSSyncProvider(filesync.StaticDirSource{"context": fsys}))
		go s.Run(ctx, func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
			c, err := net.Dial("unix", sock)
			if err != nil {
				return nil, err
			}
			req, _ := http.NewRequest("POST", "http://fwsd/session", nil)
			for k, v := range meta {
				req.Header[k] = v
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", proto)
			if err := req.Write(c); err != nil {
				return nil, err
			}
			br := bufio.NewReader(c)
			if _, err := http.ReadResponse(br, req); err != nil {
				return nil, err
			}
			return &bufConn{c, br}, nil
		})
		caller, err := sm.Get(ctx, s.ID(), false)
		if err != nil {
			t.Fatal(err)
		}
		dest := t.TempDir()
		if err := filesync.FSSync(ctx, caller, filesync.FSSendRequestOpt{Name: "context", DestDir: dest}); err != nil {
			t.Fatalf("debug %v: sync the context: %v", debug, err)
		}
		for i := 0; i < 3; i++ {
			if _, err := grpc_health_v1.NewHealthClient(caller.Conn()).Check(ctx, &grpc_health_v1.HealthCheckRequest{}); err != nil {
				t.Fatalf("debug %v: health check %d: %v", debug, i, err)
			}
		}
		s.Close()
		cancel()
		srv.Close()
	}
}
