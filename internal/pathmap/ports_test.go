package pathmap

import "testing"

func TestPortsVar(t *testing.T) {
	got, err := PortsVar(map[string]int{"80/tcp": 30001, "53/udp": 30002, "53/tcp": 30003, "9/sctp": 30004})
	if err != nil {
		t.Fatal(err)
	}
	if want := "FWS_PORTS=53/tcp=30003,53/udp=30002,80/tcp=30001"; got != want {
		t.Errorf("PortsVar: %q, want %q", got, want)
	}
	if got, err := PortsVar(nil); got != "" || err != nil {
		t.Errorf("no ports: %q, %v", got, err)
	}
	if _, err := PortsVar(map[string]int{"http/tcp": 1}); err == nil {
		t.Error("a port that is no number was accepted")
	}
}
