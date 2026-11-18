package engine

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/e6qu/fun-with-sandboxes/internal/pathmap"
)

// ioPolicyPath is the file of the container's disk I/O policy tier, which
// its processes read as they start (pathmap.IOPolicyFile).
func (c *Container) ioPolicyPath() string {
	return filepath.Join(filepath.Dir(c.pidsPath()), pathmap.IOPolicyFile)
}

// memoryPath is the file of the container's memory limit, which its
// processes give each program they start (pathmap.MemoryFile).
func (c *Container) memoryPath() string {
	return filepath.Join(filepath.Dir(c.pidsPath()), pathmap.MemoryFile)
}

// writeMemoryLimit writes the container's memory limit in MiB, as
// runtime.Limits.MemoryMiB holds it, or "max" for none.
func (c *Container) writeMemoryLimit(mib int) error {
	v := "max"
	if mib > 0 {
		v = strconv.Itoa(mib)
	}
	tmp := c.memoryPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(v+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.memoryPath())
}

// writeIOPolicy writes the tier block I/O weight weight maps to, for the
// processes the container starts from now on; a process inherits its
// parent's tier, so the spawn helper's tier stands for a container whose
// weight no update changed.
func (c *Container) writeIOPolicy(weight uint16) error {
	tmp := c.ioPolicyPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(ioPolicyFor(weight))+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.ioPolicyPath())
}
