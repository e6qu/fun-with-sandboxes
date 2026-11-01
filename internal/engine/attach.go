package engine

import (
	"context"
	"io"

	"github.com/moby/moby/api/types/container"
)

type AttachOptions struct {
	Stream bool
	// Logs replays the stored output before streaming.
	Logs           bool
	Stdout, Stderr bool
}

// Attach delivers the container's output from now on (and the stored output
// with Logs) until the current run — or, for a container not yet started,
// the next run — ends. Clients attach before start, so output the container
// writes as soon as it starts is not lost.
func (e *Engine) Attach(ctx context.Context, ref string, opts AttachOptions, fn func(LogEntry) error) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	running := c.rec.State.Status == container.StateRunning
	done, next, removed := c.runDone, c.nextRun, c.removed
	tty := c.rec.Config.Tty
	c.mu.Unlock()

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
	return c.logs.followEntries(ctx, followOptions{follow: opts.Stream, done: runEnded, tail: tail, want: want}, fn)
}

// AttachStdin feeds r to the container's standard input, waiting for the
// container to start when it has not yet.
func (e *Engine) AttachStdin(ctx context.Context, ref string, r io.Reader) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	if !c.rec.Config.OpenStdin {
		return nil
	}
	c.mu.Lock()
	w, next := c.stdin, c.nextRun
	c.mu.Unlock()
	if w == nil {
		select {
		case <-next:
		case <-ctx.Done():
			return nil
		}
		c.mu.Lock()
		w = c.stdin
		c.mu.Unlock()
	}
	if w == nil {
		return nil
	}
	_, err = io.Copy(w, r)
	if c.rec.Config.StdinOnce && !c.rec.Config.Tty {
		w.Close()
	}
	return err
}

func (c *Container) IsTTY() bool { return c.rec.Config.Tty }
