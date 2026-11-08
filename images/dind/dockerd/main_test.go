package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	o, err := Parse([]string{"-H", "tcp://0.0.0.0:2375", "--tls=false", "--host=unix:///var/run/docker.sock", "--debug"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.Hosts, []string{"tcp://0.0.0.0:2375", "unix:///var/run/docker.sock"}) || !o.Debug {
		t.Errorf("Parse = %+v", o)
	}
	if useTLS, verify := o.Secure(); useTLS || verify {
		t.Errorf("--tls=false: tls %v, verify %v", useTLS, verify)
	}
	o, err = Parse([]string{"--host=tcp://0.0.0.0:2376", "--tlsverify", "--tlscacert", "/certs/server/ca.pem", "--tlscert", "/certs/server/cert.pem", "--tlskey=/certs/server/key.pem"})
	if err != nil {
		t.Fatal(err)
	}
	if useTLS, verify := o.Secure(); !useTLS || !verify || o.CACert != "/certs/server/ca.pem" || o.Key != "/certs/server/key.pem" {
		t.Errorf("TLS flags: %+v", o)
	}
	if o, err := Parse(nil); err != nil || !reflect.DeepEqual(o.Hosts, []string{"unix:///var/run/docker.sock"}) {
		t.Errorf("no flags: %+v, %v", o, err)
	}
	for _, bad := range [][]string{{"--mtu", "1400"}, {"-H"}, {"--tls=maybe"}} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		} else if bad[0] == "--mtu" && !strings.Contains(err.Error(), "--mtu is not supported") {
			t.Errorf("Parse(%q): %v", bad, err)
		}
	}
}
