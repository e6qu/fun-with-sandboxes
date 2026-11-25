package engine

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// makeQuotaRoot puts a root of a size quota on a sparse APFS disk image of
// its own, attached at root, of the image's files' size and the quota
// beside them, and copies the staged root into it: the volume's free space
// is what the container can write.
func makeQuotaRoot(dir, staged, root string, quota int64) error {
	used, err := treeBytes(staged)
	if err != nil {
		return err
	}
	// APFS keeps a few megabytes of its own on any volume.
	size := used + quota + 32<<20
	img := quotaImage(dir)
	if out, err := exec.Command("hdiutil", "create", "-quiet", "-type", "SPARSE", "-fs", "APFS", "-volname", "fws-root",
		// hdiutil's "b" is a 512-byte sector, not a byte: the size goes in
		// kilobytes.
		"-size", strconv.FormatInt((size+1023)/1024, 10)+"k", img).CombinedOutput(); err != nil {
		return fmt.Errorf("create the root's disk image: %v: %s", err, out)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := attachQuotaRoot(dir, root); err != nil {
		return err
	}
	// ditto keeps modes, extended attributes, ACLs and file flags, as the
	// recorded owners and setuid bits are kept in them.
	if out, err := exec.Command("ditto", staged, root).CombinedOutput(); err != nil {
		detachQuotaRoot(root)
		return fmt.Errorf("copy the root into its disk image: %v: %s", err, out)
	}
	return nil
}

// attachQuotaRoot attaches a container's root disk image at root, unless it
// is attached there already, as after an engine restart without a reboot.
func attachQuotaRoot(dir, root string) error {
	if mounted(dir, root) {
		return nil
	}
	if out, err := exec.Command("hdiutil", "attach", "-quiet", "-nobrowse", "-noautoopen", "-owners", "on",
		"-mountpoint", root, quotaImage(dir)).CombinedOutput(); err != nil {
		return fmt.Errorf("attach the root's disk image: %v: %s", err, out)
	}
	return nil
}

// detachQuotaRoot detaches the disk image at root.
func detachQuotaRoot(root string) error {
	if out, err := exec.Command("hdiutil", "detach", "-quiet", "-force", root).CombinedOutput(); err != nil {
		return fmt.Errorf("detach the root's disk image: %v: %s", err, out)
	}
	return nil
}

// mounted reports whether root is a mount point below dir.
func mounted(dir, root string) bool {
	var a, b syscall.Stat_t
	return syscall.Stat(dir, &a) == nil && syscall.Stat(root, &b) == nil && a.Dev != b.Dev
}
