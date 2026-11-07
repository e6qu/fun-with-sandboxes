package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingName(t *testing.T) {
	for in, want := range map[string]string{
		"NEO4J_server_memory_pagecache_size":                  "server.memory.pagecache.size",
		"NEO4J_dbms_security_auth__minimum__password__length": "dbms.security.auth_minimum_password_length",
		"NEO4J_server_default__listen__address":               "server.default_listen_address",
		"NEO4J_apoc_export_file_enabled":                      "apoc.export.file.enabled",
	} {
		if got := SettingName(in); got != want {
			t.Errorf("SettingName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSemver(t *testing.T) {
	for _, c := range []struct {
		spec, version string
		want          bool
	}{
		{"2026.02.2", "2026.02.2", true},
		{"2026.02.x", "2026.02.2", true},
		{"2026.x", "2026.02.2", false},
		{"2026.x.x", "2026.02.2", true},
		{"2026.03.0", "2026.02.2", false},
		{"5.26.x", "2026.02.2", false},
		{"2026.02.x", "2026.02", false},
		{"2026.02.2", "", false},
	} {
		if got := Semver(c.spec, c.version); got != c.want {
			t.Errorf("Semver(%q, %q) = %v, want %v", c.spec, c.version, got, c.want)
		}
	}
}

func TestConfSettings(t *testing.T) {
	f := filepath.Join(t.TempDir(), "neo4j.conf")
	os.WriteFile(f, []byte("#server.memory.pagecache.size=10g\ndb.logs.query.enabled=INFO\nserver.jvm.additional=-XX:+UseG1GC\n"), 0o600)
	steps := []error{
		addDefault(f, "server.memory.pagecache.size", "512M"),
		addDefault(f, "db.logs.query.enabled", "OFF"),
		setSetting(f, "db.logs.query.enabled", "VERBOSE"),
		setSetting(f, "server.jvm.additional", "-Dkept=1"),
		setSetting(f, "server.memory.pagecache.size", "1G"),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(f)
	want := "#server.memory.pagecache.size=10g\nserver.jvm.additional=-XX:+UseG1GC\n\n" +
		"db.logs.query.enabled=VERBOSE\nserver.jvm.additional=-Dkept=1\nserver.memory.pagecache.size=1G\n"
	if string(b) != want {
		t.Errorf("neo4j.conf:\n%s\nwant:\n%s", b, want)
	}
}

func TestPluginConfig(t *testing.T) {
	f := filepath.Join(t.TempDir(), "neo4j.conf")
	os.WriteFile(f, []byte("dbms.security.procedures.unrestricted=my.*\n"), 0o600)
	p := plugin{Properties: map[string]string{"dbms.security.procedures.unrestricted": "apoc.*", "server.unmanaged_extension_classes": "a=/b"}}
	if err := applyPluginConfig("apoc", p, f, "#dbms.security.procedures.unrestricted=x\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	want := "dbms.security.procedures.unrestricted=apoc.*,my.*\n\nserver.unmanaged_extension_classes=a=/b\n"
	if string(b) != want {
		t.Errorf("neo4j.conf:\n%s\nwant:\n%s", b, want)
	}
}
