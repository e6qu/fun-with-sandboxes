package cri

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

// logCopy writes a container's output, as the engine stores it, to the
// log file the kubelet named, in CRI's format: each line its time, its
// stream, F for a whole line or P for part of one, and its text.
type logCopy struct {
	path   string
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	f  *os.File
}

func (l *logCopy) stop() {
	l.cancel()
	<-l.done
}

// open opens the log file at its path, after the kubelet moved the one it
// rotated away.
func (l *logCopy) open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	l.mu.Lock()
	old := l.f
	l.f = f
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func (l *logCopy) write(le engine.LogEntry) error {
	text, tag := le.Log, "P"
	if t, ok := strings.CutSuffix(text, "\n"); ok {
		text, tag = t, "F"
	}
	line := le.Time.UTC().Format(time.RFC3339Nano) + " " + le.Stream + " " + tag + " " + text + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := io.WriteString(l.f, line)
	return err
}

// startLog copies container id's output since since to logPath, a path of
// the engine's host, until the container stops.
func (s *Service) startLog(id, logPath string, since time.Time) error {
	if logPath == "" {
		return nil
	}
	host, err := s.e.HostPath(logPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &logCopy{path: host, cancel: cancel, done: make(chan struct{})}
	if err := l.open(); err != nil {
		cancel()
		return err
	}
	s.mu.Lock()
	if old := s.logs[id]; old != nil {
		s.mu.Unlock()
		cancel()
		l.f.Close()
		return nil
	}
	s.logs[id] = l
	s.mu.Unlock()
	go func() {
		defer close(l.done)
		s.e.Logs(ctx, id, engine.LogOptions{Stdout: true, Stderr: true, Follow: true, Since: since, Tail: -1}, l.write)
		l.mu.Lock()
		l.f.Close()
		l.mu.Unlock()
		s.mu.Lock()
		if s.logs[id] == l {
			delete(s.logs, id)
		}
		s.mu.Unlock()
	}()
	return nil
}

func (s *Service) stopLog(id string) {
	s.mu.Lock()
	l := s.logs[id]
	delete(s.logs, id)
	s.mu.Unlock()
	if l != nil {
		l.stop()
	}
}

// lastLogged is the time of the last line of the CRI log file at path.
func lastLogged(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	var last time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		ts, _, _ := strings.Cut(sc.Text(), " ")
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			last = t
		}
	}
	return last
}

// resumeLogs copies, as the engine opens, the output its CRI containers
// wrote since the last line their log files hold.
func (s *Service) resumeLogs() {
	for _, id := range s.listKind(kindContainer) {
		in, err := s.e.Inspect(id)
		if err != nil || in.State == nil || in.State.Status == "created" {
			continue
		}
		p := in.Config.Labels[logLabel]
		if p == "" {
			continue
		}
		host, err := s.e.HostPath(p)
		if err != nil {
			continue
		}
		since := lastLogged(host)
		if !since.IsZero() {
			since = since.Add(time.Nanosecond)
		}
		s.startLog(id, p, since)
	}
}

func (s *Service) ReopenContainerLog(ctx context.Context, req *runtimeapi.ReopenContainerLogRequest) (*runtimeapi.ReopenContainerLogResponse, error) {
	id, err := s.resolve(kindContainer, req.ContainerId)
	if err != nil {
		return nil, err
	}
	in, err := s.e.Inspect(id)
	if err != nil {
		return nil, grpcError(err)
	}
	if in.State == nil || !in.State.Running {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is not running", id)
	}
	s.mu.Lock()
	l := s.logs[id]
	s.mu.Unlock()
	if l == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s has no log", id)
	}
	if err := l.open(); err != nil {
		return nil, status.Errorf(codes.Internal, "reopen %s: %v", l.path, err)
	}
	return &runtimeapi.ReopenContainerLogResponse{}, nil
}
