package image

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// modeBits are the bits of a mode a layer carries: permissions, setuid,
// setgid and sticky.
const modeBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// TarMode is m's permission, setuid, setgid and sticky bits as a tar
// header or stat carries them.
func TarMode(m os.FileMode) int64 {
	b := int64(m.Perm())
	if m&os.ModeSetuid != 0 {
		b |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		b |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		b |= 0o1000
	}
	return b
}

// FileModeBits is the reverse of TarMode: the permission, setuid, setgid
// and sticky bits of a tar header's or stat's mode.
func FileModeBits(b int64) os.FileMode {
	m := os.FileMode(b) & os.ModePerm
	if b&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if b&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if b&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// formatMode is how the store records a mode for the container library,
// which reads it as octal and keeps 07777.
func formatMode(m os.FileMode) string { return strconv.FormatInt(TarMode(m), 8) }

// parseMode reads a recorded mode as the container library does: octal
// digits up to the first other byte, of which it keeps 07777.
func parseMode(v string) (os.FileMode, bool) {
	var m int64
	i := 0
	for ; i < len(v) && v[i] >= '0' && v[i] <= '7'; i++ {
		m = m*8 + int64(v[i]-'0')
	}
	if i == 0 {
		return 0, false
	}
	return FileModeBits(m), true
}

// formatOwner is how the container library records an owner: "uid:gid".
func formatOwner(uid, gid int) string { return strconv.Itoa(uid) + ":" + strconv.Itoa(gid) }

// parseOwner reads an owner formatOwner wrote.
func parseOwner(v string) (uid, gid int, ok bool) {
	u, g, found := strings.Cut(v, ":")
	if !found {
		return 0, 0, false
	}
	uu, err := strconv.ParseUint(u, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	gg, err := strconv.ParseUint(strings.TrimRight(g, "\x00"), 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return int(uu), int(gg), true
}

// entryMeta is the mode and owner an entry's container sees, as meta
// functions report them for asRecorded.
type entryMeta struct {
	mode     os.FileMode
	uid, gid int
}

// asRecorded copies a tar stream with each entry's mode and owner as meta
// reports them for the entry's name: root, and the mode the stream gave,
// where meta knows none. A link keeps its own mode.
func asRecorded(r io.Reader, w io.Writer, meta func(name string) (entryMeta, bool)) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if m, ok := meta(h.Name); ok {
			h.Uid, h.Gid = m.uid, m.gid
			if h.Typeflag != tar.TypeSymlink {
				h.Mode = h.Mode&^0o7777 | TarMode(m.mode)
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}
