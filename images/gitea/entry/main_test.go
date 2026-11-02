package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyEnvAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.ini")
	os.WriteFile(path, []byte("APP_NAME = x\n\n[server]\nHTTP_PORT = 1\n; comment\n[database]\nDB_TYPE = sqlite3\n"), 0o600)
	ini, err := readINI(path)
	if err != nil {
		t.Fatal(err)
	}
	applyEnv(ini, []string{"GITEA__server__ROOT_URL=http://h/", "GITEA__DEFAULT__RUN_MODE=dev", "GITEA__repository_0X2E_upload__ENABLED=false", "OTHER=1"})
	if err := ini.write(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "APP_NAME = x\nRUN_MODE = dev\n\n[server]\nHTTP_PORT = 1\nROOT_URL = http://h/\n\n[database]\nDB_TYPE = sqlite3\n\n[repository.upload]\nENABLED = false\n"
	if string(b) != want {
		t.Fatalf("app.ini:\n%s\nwant:\n%s", b, want)
	}
	if strings.Contains(string(b), "OTHER") {
		t.Fatal("a variable without the prefix reached app.ini")
	}
}
