package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\napp:x:1000:1001:App:/home/app:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc", "group"), []byte("root:x:0:\napp:x:1001:\nstaff:x:50:\naudio:x:29:root,other\nvideo:x:44:app\n"), 0o644)
	for user, want := range map[string]ID{
		"":          {0, 0, "/root", "29"},
		"root":      {0, 0, "/root", "29"},
		"app":       {1000, 1001, "/home/app", "44"},
		"1000":      {1000, 1001, "/home/app", "44"},
		"app:staff": {1000, 50, "/home/app", "44"},
		"2000":      {2000, 0, "/", ""},
		"2000:2000": {2000, 2000, "/", ""},
		"app:0":     {1000, 0, "/home/app", "44"},
	} {
		got, err := Resolve(root, user)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %+v, %v; want %+v", user, got, err, want)
		}
	}
	for _, bad := range []string{"nobody", "app:nogroup", ":0"} {
		if _, err := Resolve(root, bad); err == nil {
			t.Errorf("Resolve(%q) succeeded", bad)
		}
	}
	app, _ := Resolve(root, "app")
	if got, err := app.AddGroups(root, []string{"staff", "44", "7"}); err != nil || got.Groups != "44,50,7" {
		t.Errorf("AddGroups = %+v, %v; want groups 44,50,7", got, err)
	}
	if _, err := app.AddGroups(root, []string{"nogroup"}); err == nil {
		t.Error("AddGroups of a missing group succeeded")
	}
	// An image without /etc/passwd knows root and numeric ids.
	empty := t.TempDir()
	if got, err := Resolve(empty, "root"); err != nil || got.UID != 0 {
		t.Errorf("root without passwd: %+v, %v", got, err)
	}
	if got, err := Resolve(empty, "123:45"); err != nil || got.UID != 123 || got.GID != 45 {
		t.Errorf("numeric without passwd: %+v, %v", got, err)
	}
}
