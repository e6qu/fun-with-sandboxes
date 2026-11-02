// Package identity resolves a container's user, as Docker's user, USER and
// --user name it, against the container's own /etc/passwd and /etc/group,
// as runc does. The container library presents the identity it resolves to
// the container's processes; the host runs them as the engine's user.
package identity

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ID struct {
	UID, GID uint32
	// Home is the user's home directory in the container.
	Home string
}

// Variables the container library reads.
const (
	UIDVar = "FWS_UID"
	GIDVar = "FWS_GID"
)

// Env is the environment the container library takes the identity from.
func (id ID) Env() []string {
	return []string{UIDVar + "=" + strconv.FormatUint(uint64(id.UID), 10), GIDVar + "=" + strconv.FormatUint(uint64(id.GID), 10)}
}

// Resolve resolves user, "" for root or name-or-uid[:group-or-gid], in the
// container whose root is root.
func Resolve(root, user string) (ID, error) {
	name, group, hasGroup := strings.Cut(user, ":")
	if name == "" && hasGroup {
		return ID{}, fmt.Errorf("invalid user %q", user)
	}
	if name == "" {
		name = "0"
	}
	users, err := readEntries(filepath.Join(root, "etc", "passwd"), 7)
	if err != nil {
		return ID{}, err
	}
	var id ID
	found := false
	n, numeric := parseID(name)
	for _, e := range users {
		u, ok := parseID(e[2])
		if !ok {
			continue
		}
		if (numeric && u == n) || (!numeric && e[0] == name) {
			g, _ := parseID(e[3])
			id = ID{UID: u, GID: g, Home: e[5]}
			found = true
			break
		}
	}
	switch {
	case found:
	case numeric:
		id = ID{UID: n, Home: "/"}
		if n == 0 {
			id.Home = "/root"
		}
	case name == "root":
		id = ID{Home: "/root"}
	default:
		return ID{}, fmt.Errorf("unable to find user %s: no matching entries in passwd file", name)
	}
	if hasGroup {
		g, numeric := parseID(group)
		if !numeric {
			groups, err := readEntries(filepath.Join(root, "etc", "group"), 3)
			if err != nil {
				return ID{}, err
			}
			ok := false
			for _, e := range groups {
				if e[0] == group {
					if g, ok = parseID(e[2]); ok {
						break
					}
				}
			}
			if !ok {
				if group != "root" {
					return ID{}, fmt.Errorf("unable to find group %s: no matching entries in group file", group)
				}
				g = 0
			}
		}
		id.GID = g
	}
	return id, nil
}

func parseID(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

// readEntries reads a colon-separated database; a missing file has none.
func readEntries(path string, fields int) ([][]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < fields {
			continue
		}
		out = append(out, parts)
	}
	return out, sc.Err()
}
