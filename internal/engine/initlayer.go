package engine

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// initPaths are the paths create writes into a container's root: the hosts
// files and the mount points, which Docker keeps in a container's init
// layer.
func initPaths(mounts []mountRec) []string {
	out := []string{"/etc/hosts", "/etc/hostname"}
	for _, m := range mounts {
		out = append(out, m.Destination)
	}
	return out
}

// layInit brings init, a clone of the image root create cloned root from,
// to root's state at paths, the only ones create wrote, so init holds the
// root as create left it without a second clone of the whole tree.
func layInit(root, init string, paths []string) error {
	walked := map[string]bool{}
	for _, p := range paths {
		if err := layPath(root, init, p, walked, 0); err != nil {
			return err
		}
	}
	// What create placed in a directory changed the directory's times.
	for d := range walked {
		fi, err := os.Lstat(filepath.Join(root, d))
		if err != nil {
			return err
		}
		if err := os.Chtimes(filepath.Join(init, d), time.Time{}, fi.ModTime()); err != nil {
			return err
		}
	}
	return nil
}

// layPath copies the first component of p that differs between root and
// init, and with it everything create wrote below it, from root to init.
// A link both share leads on in the root, as rootfs.Join led create's
// writes: an absolute one from the root, a relative one from its directory.
func layPath(root, init, p string, walked map[string]bool, links int) error {
	parts := strings.Split(strings.TrimPrefix(path.Clean("/"+p), "/"), "/")
	cur := ""
	for i, part := range parts {
		cur = path.Join(cur, part)
		src, dst := filepath.Join(root, cur), filepath.Join(init, cur)
		sfi, serr := os.Lstat(src)
		dfi, derr := os.Lstat(dst)
		if i < len(parts)-1 && serr == nil && derr == nil {
			if sfi.IsDir() && dfi.IsDir() {
				walked[cur] = true
				continue
			}
			if sfi.Mode()&os.ModeSymlink != 0 && dfi.Mode()&os.ModeSymlink != 0 {
				s, serr := os.Readlink(src)
				d, derr := os.Readlink(dst)
				if serr == nil && derr == nil && s == d {
					if links >= 40 {
						return nil
					}
					from := path.Dir(cur)
					if path.IsAbs(s) {
						from = "/"
					}
					rest := path.Join(append([]string{from, s}, parts[i+1:]...)...)
					return layPath(root, init, rest, walked, links+1)
				}
			}
		}
		if derr == nil {
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		} else if !os.IsNotExist(derr) {
			return derr
		}
		if serr == nil {
			return image.CloneDir(src, dst)
		}
		if os.IsNotExist(serr) {
			return nil
		}
		return serr
	}
	return nil
}
