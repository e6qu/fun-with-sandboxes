package main

import (
	"reflect"
	"testing"
)

// The setup server's options are mongod's, with loopback, port 27017, no
// authentication, key file or, with a root user, replica set.
func TestSetupOptions(t *testing.T) {
	args := []string{"mongod", "--bind_ip_all", "--replSet", "rs", "--keyFile=/tmp/key", "--auth", "--port", "27018", "--quiet"}
	got := setArg(removeArg(removeArg(removeArg(removeArg(args, "--bind_ip_all", false), "--auth", false), "--keyFile", true), "--replSet", true), "--port", "27017")
	if want := []string{"mongod", "--quiet", "--port", "27017"}; !reflect.DeepEqual(got, want) {
		t.Errorf("setup options %q, want %q", got, want)
	}
	if !reflect.DeepEqual(args[:3], []string{"mongod", "--bind_ip_all", "--replSet"}) {
		t.Errorf("the server's own options changed: %q", args)
	}
	if !haveArg(args, "--keyFile") || haveArg(args, "--bind_ip") || argValue(args, "--replSet") != "rs" || argValue(args, "--keyFile") != "/tmp/key" {
		t.Errorf("misread %q", args)
	}
	if got := ensureArg(ensureArg([]string{"mongod"}, "--auth"), "--auth"); !reflect.DeepEqual(got, []string{"mongod", "--auth"}) {
		t.Errorf("ensureArg: %q", got)
	}
}

// The data directory and binding come from the options, then the
// configuration mongod prints, then the image's defaults.
func TestDataDirectoryAndBinding(t *testing.T) {
	config := "net:\n  bindIp: 0.0.0.0\nstorage:\n  dbPath: \"/srv/mongo\"\n"
	for _, c := range []struct {
		args   []string
		config string
		want   string
	}{
		{[]string{"mongod"}, "", "/data/db"},
		{[]string{"mongod", "--configsvr"}, "", "/data/configdb"},
		{[]string{"mongod", "--dbpath=/srv/a"}, config, "/srv/a"},
		{[]string{"mongod", "--config", "/etc/mongod.conf"}, config, "/srv/mongo"},
	} {
		if got := dbPathOf(c.args, c.config); got != c.want {
			t.Errorf("dbPathOf(%q) = %s, want %s", c.args, got, c.want)
		}
	}
	if !configBinds(config) || configBinds("storage:\n  dbPath: /x\n") {
		t.Error("configBinds misreads the configuration")
	}
	if jsString(`a"b`) != `"a\"b"` {
		t.Errorf("jsString: %s", jsString(`a"b`))
	}
}
