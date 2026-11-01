package image

import (
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// Credentials are registry credentials by server address, as the Docker CLI
// sends them in X-Registry-Config for a build.
type Credentials map[string]authn.AuthConfig

// For returns the authenticator for the registry ref names, anonymous when
// the credentials hold none for it.
func (c Credentials) For(ref string) authn.Authenticator {
	r, err := name.ParseReference(ref)
	if err != nil {
		return authn.Anonymous
	}
	want := r.Context().RegistryStr()
	for addr, cfg := range c {
		if RegistryHost(addr) == want {
			return Authenticator(cfg)
		}
	}
	return authn.Anonymous
}

// RegistryHost reduces a server address as Docker writes it, such as
// "https://index.docker.io/v1/", to the registry host go-containerregistry
// names, such as "index.docker.io".
func RegistryHost(addr string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	if h == "" || h == "docker.io" || h == "registry-1.docker.io" {
		return name.DefaultRegistry
	}
	return h
}

// Authenticator turns credentials into an authenticator, anonymous when they
// hold nothing.
func Authenticator(cfg authn.AuthConfig) authn.Authenticator {
	if cfg == (authn.AuthConfig{}) {
		return authn.Anonymous
	}
	return authn.FromConfig(cfg)
}
