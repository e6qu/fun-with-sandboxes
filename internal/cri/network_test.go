package cri

import "testing"

func TestPodSubnet(t *testing.T) {
	for in, want := range map[string]string{
		"10.244.1.0/24":              "10.244.1.0/24",
		"10.244.1.0/24,fd00:1::/64":  "10.244.1.0/24",
		"fd00:1::/64, 10.244.2.0/24": "10.244.2.0/24",
	} {
		got, err := podSubnet(in)
		if err != nil || got.String() != want {
			t.Errorf("podSubnet(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "fd00:1::/64", "10.244.1.5/24", "not a cidr"} {
		if got, err := podSubnet(in); err == nil {
			t.Errorf("podSubnet(%q) = %v, want an error", in, got)
		}
	}
}
