package image

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
)

func TestCredentialsForRegistry(t *testing.T) {
	c := Credentials{
		"https://index.docker.io/v1/": {Username: "hub"},
		"127.0.0.1:5000":              {Username: "local"},
	}
	for ref, want := range map[string]string{
		"postgres:16":                "hub",
		"docker.io/library/redis":    "hub",
		"127.0.0.1:5000/a/b:1":       "local",
		"ghcr.io/e6qu/fws-toolbox:1": "",
	} {
		a := c.For(ref)
		if want == "" {
			if a != authn.Anonymous {
				t.Errorf("For(%q) = %v, want anonymous", ref, a)
			}
			continue
		}
		cfg, err := a.Authorization()
		if err != nil || cfg.Username != want {
			t.Errorf("For(%q) = %+v, %v; want %s", ref, cfg, err, want)
		}
	}
}
