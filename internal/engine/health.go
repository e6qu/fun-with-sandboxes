package engine

import (
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
)

// Docker's defaults for a health check that leaves a field unset.
const (
	defaultHealthInterval      = 30 * time.Second
	defaultHealthTimeout       = 30 * time.Second
	defaultHealthStartInterval = 5 * time.Second
	defaultHealthRetries       = 3
	healthLogEntries           = 5
)

func itoa(n int) string { return strconv.Itoa(n) }

func healthArgv(test []string) []string {
	switch test[0] {
	case "CMD":
		return test[1:]
	case "CMD-SHELL":
		return []string{"/bin/sh", "-c", test[1]}
	}
	return test
}

// monitorHealth runs the container's health check on its configured cadence
// until stop or done closes.
func (e *Engine) monitorHealth(c *Container, hc container.HealthConfig, stop, done <-chan struct{}) {
	interval, timeout := hc.Interval, hc.Timeout
	if interval == 0 {
		interval = defaultHealthInterval
	}
	if timeout == 0 {
		timeout = defaultHealthTimeout
	}
	startInterval := hc.StartInterval
	if startInterval == 0 {
		startInterval = defaultHealthStartInterval
	}
	retries := hc.Retries
	if retries == 0 {
		retries = defaultHealthRetries
	}
	argv := healthArgv(hc.Test)
	started := time.Now()
	inStartPeriod := func() bool { return hc.StartPeriod > 0 && time.Since(started) < hc.StartPeriod }

	for {
		wait := interval
		c.mu.Lock()
		starting := c.health != nil && c.health.Status == container.Starting
		c.mu.Unlock()
		if starting && inStartPeriod() {
			wait = startInterval
		}
		select {
		case <-stop:
			return
		case <-done:
			return
		case <-e.detach:
			return
		case <-time.After(wait):
		}
		if c.isPaused() {
			continue
		}
		begin := time.Now()
		code, output := e.probe(c, argv, timeout)
		res := &container.HealthcheckResult{Start: begin, End: time.Now(), ExitCode: code, Output: output}

		c.mu.Lock()
		if c.health == nil || c.healthStop == nil {
			c.mu.Unlock()
			return
		}
		h := c.health
		prev := h.Status
		h.Log = append(h.Log, res)
		if len(h.Log) > healthLogEntries {
			h.Log = h.Log[len(h.Log)-healthLogEntries:]
		}
		if code == 0 {
			h.Status = container.Healthy
			h.FailingStreak = 0
		} else if !(inStartPeriod() && h.Status == container.Starting) {
			// Failures inside the start period do not count, as in Docker.
			h.FailingStreak++
			if h.FailingStreak >= retries {
				h.Status = container.Unhealthy
			}
		}
		status := h.Status
		c.mu.Unlock()
		if status != prev {
			e.publishContainer(c, events.Action("health_status: "+string(status)), nil)
		}
	}
}
