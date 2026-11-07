package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRunsInfluxd(t *testing.T) {
	for _, c := range []struct {
		args []string
		runs bool
		rest []string
	}{
		{nil, true, nil},
		{[]string{"influxd"}, true, []string{}},
		{[]string{"run", "--log-level=debug"}, true, []string{"--log-level=debug"}},
		{[]string{"--reporting-disabled"}, true, []string{"--reporting-disabled"}},
		{[]string{"influxd", "run"}, true, []string{}},
		{[]string{"influxd", "--http-bind-address", ":9000"}, true, []string{"--http-bind-address", ":9000"}},
		{[]string{"influxd", "print-config"}, false, []string{"influxd", "print-config"}},
		{[]string{"influx", "version"}, false, []string{"influx", "version"}},
	} {
		runs, rest := RunsInfluxd(c.args)
		if runs != c.runs || (len(rest) != 0 || len(c.rest) != 0) && !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("RunsInfluxd(%q) = %v %q, want %v %q", c.args, runs, rest, c.runs, c.rest)
		}
	}
}

func TestConfigGet(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.toml")
	os.WriteFile(conf, []byte("bolt-path = \"/data/a.bolt\"\nlog-level = \"debug\"\n"), 0o644)
	t.Setenv("SANDBOX_ROOT", "/")
	t.Setenv("INFLUXD_CONFIG_PATH", conf)
	t.Setenv("INFLUXD_ENGINE_PATH", "")
	for _, c := range []struct {
		key  string
		args []string
		want string
	}{
		{"bolt-path", nil, "/data/a.bolt"},
		{"bolt-path", []string{"--bolt-path=/x.bolt"}, "/x.bolt"},
		{"bolt-path", []string{"--engine-path", "/e", "--bolt-path", "/y.bolt"}, "/y.bolt"},
		{"engine-path", nil, ""},
		{"http-bind-address", nil, ":8086"},
		{"log-level", nil, "debug"},
	} {
		if got := ConfigGet(c.key, c.args); got != c.want {
			t.Errorf("ConfigGet(%q, %q) = %q, want %q", c.key, c.args, got, c.want)
		}
	}
}

func TestInitConfig(t *testing.T) {
	t.Setenv("SANDBOX_ROOT", "/")
	conf := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(conf, []byte("bolt-path: /var/lib/influxdb2/influxd.bolt\ntls-cert: /c.pem\ntls-key: /k.pem\nhttp-bind-address: :8086\n"), 0o644)
	b, err := InitConfig(conf, ":9999")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"bolt-path": "/var/lib/influxdb2/influxd.bolt", "http-bind-address": ":9999"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InitConfig = %v, want %v", got, want)
	}
}

func TestLineAndFields(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	if got, want := Line(at, "info", "pinging influxd...", "ping_attempt", "0"),
		"2026-01-02T03:04:05.000000006Z\tinfo\tpinging influxd...\t{\"system\": \"docker\", \"ping_attempt\": \"0\"}"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	if got := FirstFields("0a1\tadmin\n0b2\tother\n"); got != "0a1\n0b2" {
		t.Errorf("FirstFields = %q", got)
	}
	for bind, want := range map[string]string{":8086": "8086", "0.0.0.0:9000": "9000"} {
		if got := FinalPort(bind); got != want {
			t.Errorf("FinalPort(%q) = %q, want %q", bind, got, want)
		}
	}
}
