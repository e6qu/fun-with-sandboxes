package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/libsystem"
)

// defaultShmSize is the size of a container's shared memory when it asks
// for none, as Docker's daemon gives its /dev/shm.
const defaultShmSize = 64 << 20

// ShmVar gives the container library the ledger of the container's POSIX
// shared memory objects and their size limit, a tab apart.
const ShmVar = "FWS_SHM"

// shmOwner is the container whose IPC namespace holds c's POSIX shared
// memory and sets its size, or nil on the host's or with none.
func (e *Engine) shmOwner(c *Container) (*Container, error) {
	mode := string(c.rec.HostConfig.IpcMode)
	switch {
	case mode == "" || mode == "private" || mode == "shareable":
		return c, nil
	case strings.HasPrefix(mode, "container:"):
		return e.Container(strings.TrimPrefix(mode, "container:"))
	}
	return nil, nil
}

// shmLedger is the file in which the container library records the sizes
// of the POSIX shared memory objects of c's IPC namespace.
func (c *Container) shmLedger() string { return filepath.Join(c.dir, "shm") }

// shmEnv is the variable that has the library hold c's shared memory to
// its IPC namespace's size, and the ledger it writes, or nothing.
func (e *Engine) shmEnv(c *Container) (string, string, error) {
	owner, err := e.shmOwner(c)
	if err != nil || owner == nil || owner.rec.HostConfig.ShmSize <= 0 {
		return "", "", err
	}
	l := owner.shmLedger()
	return fmt.Sprintf("%s=%s\t%d", ShmVar, l, owner.rec.HostConfig.ShmSize), l, nil
}

// clearShm removes the shared memory objects c's last run left, as each
// run of a Docker container starts with an empty /dev/shm.
func clearShm(c *Container) error {
	f, err := os.Open(c.shmLedger())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, _, _ := strings.Cut(sc.Text(), "\t")
		if name != "" {
			libsystem.ShmUnlink(name)
		}
	}
	f.Close()
	return os.Remove(c.shmLedger())
}
