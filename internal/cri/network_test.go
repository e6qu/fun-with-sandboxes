package cri

import (
	"fmt"
	"testing"
)

func TestPodSubnets(t *testing.T) {
	for in, want := range map[string]string{
		"10.244.1.0/24":              "[10.244.1.0/24]",
		"10.244.1.0/24,fd00:1::/64":  "[10.244.1.0/24 fd00:1::/64]",
		"fd00:1::/64, 10.244.2.0/24": "[10.244.2.0/24 fd00:1::/64]",
		"fd00:10:244:1::/64":         "[fd00:10:244:1::/64]",
	} {
		got, err := podSubnets(in)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("podSubnets(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "10.244.1.5/24", "fd00:1::5/64", "not a cidr", "10.244.1.0/24,10.244.2.0/24", "fd00:1::/64,fd00:2::/64"} {
		if got, err := podSubnets(in); err == nil {
			t.Errorf("podSubnets(%q) = %v, want an error", in, got)
		}
	}
}
