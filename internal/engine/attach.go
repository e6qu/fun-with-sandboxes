package engine

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/term"
)

// defaultDetachKeys are ctrl-p ctrl-q, which detach a client from a
// container's or an exec's terminal when it names no keys of its own.
var defaultDetachKeys = []byte{16, 17}

// parseDetachKeys reads a client's detach keys, as Docker's --detach-keys
// and the CLI's detachKeys setting spell them: comma-separated characters
// or ctrl- combinations.
func parseDetachKeys(s string) ([]byte, bool) {
	if s == "" {
		return nil, true
	}
	keys, err := term.ToBytes(s)
	return keys, err == nil
}

// escapable reads r until the detach keys arrive, which it holds back and
// for which it returns term.EscapeError.
func escapable(r io.Reader, keys []byte) io.Reader {
	if len(keys) == 0 {
		keys = defaultDetachKeys
	}
	return term.NewEscapeProxy(r, keys)
}

func detachedBy(err error) bool {
	var esc term.EscapeError
	return errors.As(err, &esc)
}

type AttachOptions struct {
	Stream bool
	// Logs replays the stored output before streaming.
	Logs                  bool
	Stdin, Stdout, Stderr bool
	// DetachKeys end the attach, the container running on, when the client
	// sends them on a terminal's input.
	DetachKeys string
}

// AttachStreams opens the client's side of an attach once the engine has
// accepted it: its input, read when the attach takes stdin, and the
// writer of the container's output.
type AttachStreams func() (io.Reader, func(LogEntry) error, error)

// Attach delivers the container's output from now on (and the stored output
// with Logs) until the current run — or, for a container not yet started,
// the next run — ends, and feeds the client's input to its standard input.
// Clients attach before start, so output the container writes as soon as
// it starts is not lost. An attach to a terminal ends when the client sends
// the detach keys.
func (e *Engine) Attach(ctx context.Context, ref string, opts AttachOptions, open AttachStreams) error {
	keys, ok := parseDetachKeys(opts.DetachKeys)
	if !ok {
		return invalid("Invalid detach keys (%s) provided", opts.DetachKeys)
	}
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	running := c.rec.State.Status == container.StateRunning
	restarting := c.rec.State.Status == container.StateRestarting
	paused := c.rec.State.Paused
	done, next, removed := c.runDone, c.nextRun, c.removed
	tty := c.rec.Config.Tty
	c.mu.Unlock()
	if paused {
		return conflict("container %s is paused, unpause the container before attach", ref)
	}
	if restarting {
		return conflict("container %s is restarting, wait until the container is running", ref)
	}
	in, out, err := open()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runEnded := make(chan struct{})
	go func() {
		defer close(runEnded)
		if !running {
			select {
			case <-next:
				c.mu.Lock()
				done = c.runDone
				c.mu.Unlock()
			case <-removed:
				return
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()

	tail := 0
	if opts.Logs {
		tail = -1
	}
	want := func(le LogEntry) bool {
		return tty || le.Stream == "stdout" && opts.Stdout || le.Stream == "stderr" && opts.Stderr
	}
	e.publishContainer(c, events.ActionAttach, nil)
	var detached atomic.Bool
	if opts.Stream && opts.Stdin && in != nil {
		if tty {
			in = escapable(in, keys)
		}
		go func() {
			if detachedBy(e.feedStdin(ctx, c, in)) {
				detached.Store(true)
				cancel()
			}
		}()
	}
	err = c.logs.followEntries(ctx, followOptions{follow: opts.Stream, done: runEnded, tail: tail, want: want}, out)
	if detached.Load() {
		e.publishContainer(c, events.ActionDetach, nil)
		return nil
	}
	return err
}

// feedStdin feeds r to the container's standard input, waiting for the
// container to start when it has not yet.
func (e *Engine) feedStdin(ctx context.Context, c *Container, r io.Reader) error {
	if !c.rec.Config.OpenStdin {
		return nil
	}
	c.mu.Lock()
	task, next := c.task, c.nextRun
	c.mu.Unlock()
	if task == nil {
		select {
		case <-next:
		case <-ctx.Done():
			return nil
		}
		c.mu.Lock()
		task = c.task
		c.mu.Unlock()
	}
	if task == nil {
		return nil
	}
	w, err := task.Stdin(c.rec.Config.StdinOnce && !c.rec.Config.Tty)
	if err != nil {
		// The run has ended, or its input is closed: there is nothing to
		// feed, as with Docker.
		return nil
	}
	_, err = io.Copy(w, r)
	w.Close()
	return err
}

// detachable passes writes on to w until it is detached, and drops them
// after, so a terminal whose client detached keeps draining.
type detachable struct {
	mu  sync.Mutex
	w   io.Writer
	off bool
}

func (d *detachable) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.off {
		return len(p), nil
	}
	return d.w.Write(p)
}

// detach returns once no write reaches w any more.
func (d *detachable) detach() {
	d.mu.Lock()
	d.off = true
	d.mu.Unlock()
}

func (c *Container) IsTTY() bool { return c.rec.Config.Tty }
