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
	// Groups are the supplementary groups, comma-separated: those of the
	// container's /etc/group that list the user as a member, as runc
	// gives them.
	Groups string
}

// Variables the container library reads.
const (
	UIDVar    = "FWS_UID"
	GIDVar    = "FWS_GID"
	GroupsVar = "FWS_GROUPS"
)

// Env is the environment the container library takes the identity from.
func (id ID) Env() []string {
	return []string{UIDVar + "=" + strconv.FormatUint(uint64(id.UID), 10), GIDVar + "=" + strconv.FormatUint(uint64(id.GID), 10),
		GroupsVar + "=" + id.Groups}
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
	userName := ""
	n, numeric := parseID(name)
	for _, e := range users {
		u, ok := parseID(e[2])
		if !ok {
			continue
		}
		if (numeric && u == n) || (!numeric && e[0] == name) {
			g, _ := parseID(e[3])
			id = ID{UID: u, GID: g, Home: e[5]}
			userName = e[0]
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
		userName = "root"
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
	if userName != "" {
		groups, err := readEntries(filepath.Join(root, "etc", "group"), 4)
		if err != nil {
			return ID{}, err
		}
		var gids []uint32
		for _, e := range groups {
			g, ok := parseID(e[2])
			if !ok {
				continue
			}
			for _, m := range strings.Split(e[3], ",") {
				if m == userName {
					gids = append(gids, g)
					break
				}
			}
		}
		id.Groups = joinIDs(gids)
	}
	return id, nil
}

// AddGroups adds the groups that Docker's --group-add names, by name or
// id, to the supplementary groups.
func (id ID) AddGroups(root string, add []string) (ID, error) {
	if len(add) == 0 {
		return id, nil
	}
	groups, err := readEntries(filepath.Join(root, "etc", "group"), 3)
	if err != nil {
		return ID{}, err
	}
	var gids []uint32
	if id.Groups != "" {
		for _, s := range strings.Split(id.Groups, ",") {
			g, _ := parseID(s)
			gids = append(gids, g)
		}
	}
	for _, a := range add {
		g, numeric := parseID(a)
		if !numeric {
			found := false
			for _, e := range groups {
				if e[0] == a {
					g, found = parseID(e[2])
					break
				}
			}
			if !found {
				return ID{}, fmt.Errorf("unable to find group %s: no matching entries in group file", a)
			}
		}
		seen := false
		for _, x := range gids {
			seen = seen || x == g
		}
		if !seen {
			gids = append(gids, g)
		}
	}
	id.Groups = joinIDs(gids)
	return id, nil
}

func joinIDs(ids []uint32) string {
	s := make([]string, len(ids))
	for i, g := range ids {
		s[i] = strconv.FormatUint(uint64(g), 10)
	}
	return strings.Join(s, ",")
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
