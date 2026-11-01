package engine

import (
	"errors"
	"syscall"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
)

// signalGroups delivers sig to every process group of the container.
func (e *Engine) signalGroups(c *Container, sig syscall.Signal) error {
	for g := range e.groups(c) {
		if err := syscall.Kill(-g, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

// Pause stops every process of the container with SIGSTOP, the closest an
// unprivileged process gets to freezing a cgroup.
func (e *Engine) Pause(ref string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	switch {
	case c.rec.State.Status != container.StateRunning:
		c.mu.Unlock()
		return conflict("container %s is not running", c.rec.ID)
	case c.paused:
		c.mu.Unlock()
		return conflict("container %s is already paused", c.rec.ID)
	}
	c.paused = true
	c.mu.Unlock()
	if err := e.signalGroups(c, syscall.SIGSTOP); err != nil {
		return err
	}
	e.publishContainer(c, events.ActionPause, nil)
	return nil
}

func (e *Engine) Unpause(ref string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if !c.paused {
		c.mu.Unlock()
		return conflict("container %s is not paused", c.rec.ID)
	}
	c.paused = false
	c.mu.Unlock()
	if err := e.signalGroups(c, syscall.SIGCONT); err != nil {
		return err
	}
	e.publishContainer(c, events.ActionUnPause, nil)
	return nil
}

func (c *Container) isPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// stateName is the container's state as Docker reports it.
func (c *Container) stateName() container.ContainerState {
	if c.paused && c.rec.State.Status == container.StateRunning {
		return container.StatePaused
	}
	return c.rec.State.Status
}

// Update changes the restart policy; resource limits cannot be enforced
// without root, so requests for them fail.
func (e *Engine) Update(ref string, req container.UpdateConfig) (container.UpdateResponse, error) {
	c, err := e.Container(ref)
	if err != nil {
		return container.UpdateResponse{}, err
	}
	if err := checkHostConfig(&container.HostConfig{Resources: req.Resources}); err != nil {
		return container.UpdateResponse{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.RestartPolicy.Name != "" {
		if c.rec.HostConfig.AutoRemove && req.RestartPolicy.Name != container.RestartPolicyDisabled {
			return container.UpdateResponse{}, invalid("Restart policy cannot be updated because AutoRemove is enabled for the container")
		}
		c.rec.HostConfig.RestartPolicy = req.RestartPolicy
	}
	if err := c.save(); err != nil {
		return container.UpdateResponse{}, err
	}
	e.publishContainer(c, events.ActionUpdate, nil)
	return container.UpdateResponse{Warnings: []string{}}, nil
}
