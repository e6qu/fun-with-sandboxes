package engine

import (
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
)

// Pause stops every process of the container with SIGSTOP, the closest an
// unprivileged process gets to freezing a cgroup; the shim stops them, so
// it does not continue them as it throttles CPU.
func (e *Engine) Pause(ref string) error {
	c, err := e.Container(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	switch {
	case c.rec.State.Status != container.StateRunning || c.task == nil:
		c.mu.Unlock()
		return conflict("container %s is not running", c.rec.ID)
	case c.rec.State.Paused:
		c.mu.Unlock()
		return conflict("container %s is already paused", c.rec.ID)
	}
	task := c.task
	c.rec.State.Paused = true
	c.save()
	c.mu.Unlock()
	if err := task.Pause(); err != nil {
		c.mu.Lock()
		c.rec.State.Paused = false
		c.save()
		c.mu.Unlock()
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
	if !c.rec.State.Paused || c.task == nil {
		c.mu.Unlock()
		return conflict("container %s is not paused", c.rec.ID)
	}
	task := c.task
	c.rec.State.Paused = false
	c.save()
	c.mu.Unlock()
	if err := task.Unpause(); err != nil {
		return err
	}
	e.publishContainer(c, events.ActionUnPause, nil)
	return nil
}

func (c *Container) isPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rec.State.Paused
}

// stateName is the container's state as Docker reports it.
func (c *Container) stateName() container.ContainerState {
	if c.rec.State.Paused && c.rec.State.Status == container.StateRunning {
		return container.StatePaused
	}
	return c.rec.State.Status
}

// Update changes the restart policy and the resource limits; the shim of a
// running container applies new limits at once. A field the request leaves
// zero keeps its value, as with dockerd.
func (e *Engine) Update(ref string, req container.UpdateConfig) (container.UpdateResponse, error) {
	c, err := e.Container(ref)
	if err != nil {
		return container.UpdateResponse{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.RestartPolicy.Name != "" && c.rec.HostConfig.AutoRemove && req.RestartPolicy.Name != container.RestartPolicyDisabled {
		return container.UpdateResponse{}, invalid("Restart policy cannot be updated because AutoRemove is enabled for the container")
	}
	r, u := c.rec.HostConfig.Resources, req.Resources
	running := c.task != nil && c.rec.State.Status == container.StateRunning
	if u.Memory != 0 {
		r.Memory = u.Memory
	}
	if u.MemorySwap != 0 {
		r.MemorySwap = u.MemorySwap
	}
	if u.MemoryReservation != 0 {
		r.MemoryReservation = u.MemoryReservation
	}
	if u.NanoCPUs != 0 {
		r.NanoCPUs, r.CPUQuota, r.CPUPeriod = u.NanoCPUs, 0, 0
	}
	if u.CPUQuota != 0 || u.CPUPeriod != 0 {
		r.NanoCPUs = 0
		if u.CPUQuota != 0 {
			r.CPUQuota = u.CPUQuota
		}
		if u.CPUPeriod != 0 {
			r.CPUPeriod = u.CPUPeriod
		}
	}
	if u.CPUShares != 0 {
		r.CPUShares = u.CPUShares
	}
	if u.PidsLimit != nil {
		r.PidsLimit = u.PidsLimit
	}
	if u.BlkioWeight != 0 {
		if running {
			return container.UpdateResponse{}, unsupported("the block I/O weight of a running container cannot change: macOS lets a process set only its own disk I/O policy, which its children inherit as it starts them")
		}
		r.BlkioWeight = u.BlkioWeight
	}
	if u.CpusetCpus != "" || u.CpusetMems != "" || u.CPURealtimePeriod != 0 || u.CPURealtimeRuntime != 0 ||
		len(u.BlkioWeightDevice) > 0 || len(u.BlkioDeviceReadBps) > 0 || len(u.BlkioDeviceWriteBps) > 0 ||
		len(u.BlkioDeviceReadIOps) > 0 || len(u.BlkioDeviceWriteIOps) > 0 || len(u.Devices) > 0 ||
		u.MemorySwappiness != nil || u.OomKillDisable != nil {
		// Let the check refuse what cannot be enforced.
		if _, _, err := containerLimits(u); err != nil {
			return container.UpdateResponse{}, err
		}
	}
	limits, res, err := containerLimits(r)
	if err != nil {
		return container.UpdateResponse{}, err
	}
	if running {
		var nice *int
		if u.CPUShares != 0 {
			nice = &limits.Nice
		}
		if err := c.task.Update(res, nice); err != nil {
			return container.UpdateResponse{}, err
		}
	}
	c.rec.HostConfig.Resources = r
	if req.RestartPolicy.Name != "" {
		c.rec.HostConfig.RestartPolicy = req.RestartPolicy
	}
	if err := c.save(); err != nil {
		return container.UpdateResponse{}, err
	}
	e.publishContainer(c, events.ActionUpdate, nil)
	return container.UpdateResponse{Warnings: []string{}}, nil
}
