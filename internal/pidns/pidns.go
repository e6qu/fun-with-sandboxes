// Package pidns presents a PID namespace that containers share, as
// --pid container:<name> makes one. Seatbelt lets a process signal and
// inspect the processes of its own sandbox alone, and a running container's
// sandbox takes in no process of another's, so each container's shim
// carries what crosses between them: it lists the namespace's processes,
// signals them and reads their information for its container's library,
// which asks it over a Unix socket only that container reaches.
//
// The namespace's file, which the engine keeps and no container may write,
// lists the tags (SANDBOX_ROOT=<root>) of the containers that share it, the
// one whose main process is the namespace's pid 1 first.
package pidns

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The operations a request names. A request is 24 bytes, in the machine's
// byte order: the operation, the pid, a, size, each four bytes, and b,
// eight. The reply is the result and an errno, four bytes each, followed by
// as many bytes of data as a positive result counts for the operations
// that return data.
const (
	// OpList lists the namespace's processes: the result counts them, and
	// the data holds their host pids, four bytes each.
	OpList = 1
	// OpKill sends signal a to pid, as kill does: to a process, a process
	// group when pid is below -1, or with -1 to every process but the
	// caller, b, and the namespace's pid 1, size.
	OpKill = 2
	// OpInfo is proc_pidinfo of pid, flavor a, argument b, into size bytes.
	OpInfo = 3
	// OpPath is proc_pidpath of pid.
	OpPath = 4
	// OpArgs is pid's kern.procargs2.
	OpArgs = 5
)

// RequestSize and ReplySize are the sizes of a request and of a reply's
// result and errno.
const (
	RequestSize = 24
	ReplySize   = 8
)

// MaxData bounds the data a reply carries.
const MaxData = 1 << 20

// Tags reads the namespace's file.
func Tags(file string) ([]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(l) > 0 {
			out = append(out, string(l))
		}
	}
	return out, nil
}

// Write replaces the namespace's file with tags, at once, so its readers
// see the old list or the new one.
func Write(file string, tags []string) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, t := range tags {
		if strings.Contains(t, "\n") {
			tmp.Close()
			os.Remove(tmp.Name())
			return errors.New("pidns: a tag holds a newline")
		}
		b.WriteString(t + "\n")
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// Join adds tag to the namespace's file.
func Join(file, tag string) error {
	tags, err := Tags(file)
	if err != nil {
		return err
	}
	for _, t := range tags {
		if t == tag {
			return nil
		}
	}
	return Write(file, append(tags, tag))
}

// Leave takes tag out of the namespace's file; a file already gone, its
// namespace ended, is left so.
func Leave(file, tag string) error {
	tags, err := Tags(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	out := tags[:0]
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return Write(file, out)
}
