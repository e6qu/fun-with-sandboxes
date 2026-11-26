package main

import (
	"slices"
	"testing"
)

// A template's references to variables that are set are substituted, and
// nginx's own variables, which are not, stay as written.
func TestSubstituteSetVariablesOnly(t *testing.T) {
	env := map[string]string{"NGINX_HOST": "example.test", "PORT": "8080"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	in := "server_name ${NGINX_HOST}; listen $PORT; return 301 https://$host$request_uri; set $x ${UNSET};"
	want := "server_name example.test; listen 8080; return 301 https://$host$request_uri; set $x ${UNSET};"
	if got := substitute(in, lookup); got != want {
		t.Errorf("substitute:\n got %q\nwant %q", got, want)
	}
}

// nginx runs in the foreground unless the command's own arguments set -g,
// and those arguments reach nginx after the container's paths.
func TestNginxArgs(t *testing.T) {
	base := nginxArgs("/c", nil)
	if !slices.Equal(base[len(base)-2:], []string{"-g", "daemon off;"}) {
		t.Errorf("no arguments: %q, want -g 'daemon off;' last", base)
	}
	if got := nginxArgs("/c", []string{"-T"}); !slices.Equal(got[len(got)-3:], []string{"-g", "daemon off;", "-T"}) {
		t.Errorf("-T: %q", got)
	}
	got := nginxArgs("/c", []string{"-g", "daemon off; worker_processes 2;"})
	if n := slices.Index(got, "-g"); n < 0 || got[n+1] != "daemon off; worker_processes 2;" || slices.Index(got[n+1:], "-g") >= 0 {
		t.Errorf("own -g: %q, want it alone", got)
	}
}
