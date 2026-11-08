package cri

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/cri-streaming/pkg/streaming"
	remotecommandserver "k8s.io/cri-streaming/pkg/streaming/remotecommand"
	utilexec "k8s.io/utils/exec"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// openStreams serves exec, attach and port-forward streams, whose URLs
// Exec, Attach and PortForward hand the kubelet, as containerd's stream
// server does.
func (s *Service) openStreams() error {
	l, addr, err := s.e.ListenStreams()
	if err != nil {
		return err
	}
	cfg := streaming.DefaultConfig
	cfg.Addr = addr
	cfg.BaseURL = &url.URL{Scheme: "http", Host: addr}
	srv, err := streaming.NewServer(cfg, streamRuntime{s})
	if err != nil {
		l.Close()
		return err
	}
	s.streams = srv
	s.streamHTTP = &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	go s.streamHTTP.Serve(l)
	return nil
}

func (s *Service) streamServer() (streaming.Server, error) {
	if s.streams == nil {
		return nil, status.Errorf(codes.Unavailable, "the runtime serves no streams: %v", s.streamsErr)
	}
	return s.streams, nil
}

func (s *Service) running(id string) error {
	in, err := s.e.Inspect(id)
	if err != nil {
		return grpcError(err)
	}
	if in.State == nil || !in.State.Running {
		return status.Errorf(codes.FailedPrecondition, "container %s is not running", id)
	}
	return nil
}

func (s *Service) Exec(ctx context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	srv, err := s.streamServer()
	if err != nil {
		return nil, err
	}
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	if err := s.running(id); err != nil {
		return nil, err
	}
	req.ContainerId = id
	return srv.GetExec(req)
}

func (s *Service) Attach(ctx context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	srv, err := s.streamServer()
	if err != nil {
		return nil, err
	}
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	if err := s.running(id); err != nil {
		return nil, err
	}
	req.ContainerId = id
	return srv.GetAttach(req)
}

func (s *Service) PortForward(ctx context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	srv, err := s.streamServer()
	if err != nil {
		return nil, err
	}
	id, err := s.resolve(kindSandbox, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	req.PodSandboxId = id
	return srv.GetPortForward(req)
}

// streamRuntime runs what the stream server's clients ask for on the
// engine.
type streamRuntime struct{ s *Service }

func (r streamRuntime) Exec(ctx context.Context, id string, cmd []string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommandserver.TerminalSize) error {
	e := r.s.e
	x, err := e.ExecCreate(id, container.ExecCreateRequest{
		Cmd: cmd, Tty: tty, AttachStdin: in != nil, AttachStdout: out != nil, AttachStderr: errw != nil,
	})
	if err != nil {
		return err
	}
	go func() {
		for sz := range resize {
			e.ExecResize(x, sz.Height, sz.Width)
		}
	}()
	stdio := engine.ExecIO{}
	if in != nil {
		stdio.Stdin = in
	}
	if out != nil {
		stdio.Stdout = out
	}
	if errw != nil {
		stdio.Stderr = errw
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			r.s.killExec(x)
		case <-done:
		}
	}()
	if err := e.ExecStart(x, tty, nil, stdio); err != nil {
		return err
	}
	info, err := e.ExecInspect(x)
	if err != nil {
		return err
	}
	if info.ExitCode != nil && *info.ExitCode != 0 {
		return utilexec.CodeExitError{Err: fmt.Errorf("command %q exited with %d", cmd, *info.ExitCode), Code: *info.ExitCode}
	}
	return nil
}

func (r streamRuntime) Attach(ctx context.Context, id string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommandserver.TerminalSize) error {
	e := r.s.e
	go func() {
		for sz := range resize {
			e.Resize(id, sz.Height, sz.Width)
		}
	}()
	opts := engine.AttachOptions{Stream: true, Stdin: in != nil, Stdout: out != nil, Stderr: errw != nil}
	return e.Attach(ctx, id, opts, func() (io.Reader, func(engine.LogEntry) error, error) {
		return in, func(le engine.LogEntry) error {
			var w io.Writer
			switch {
			case le.Stream == "stderr" && errw != nil:
				w = errw
			case out != nil:
				w = out
			default:
				return nil
			}
			_, err := io.WriteString(w, le.Log)
			return err
		}, nil
	})
}

// PortForward connects stream to the pod's port, where a process of its
// network listens, until the pod's side closes the connection or the
// client goes.
func (r streamRuntime) PortForward(ctx context.Context, sandbox string, port int32, stream io.ReadWriteCloser) error {
	conn, err := r.s.e.DialPort(sandbox, int(port))
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, stream)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	back := make(chan error, 1)
	go func() {
		_, err := io.Copy(stream, conn)
		back <- err
	}()
	select {
	case err := <-back:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
