package api

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// A container whose NetworkMode and EndpointsConfig name its network by ID,
// as Testcontainers for Java names it, joins it once, with the aliases its
// endpoint asked for.
func TestCreateOnANetworkByID(t *testing.T) {
	ts, e := newEngineServer(t)
	loadImage(t, e, "fws/tiny:1")
	var n network.CreateResponse
	if r := call(t, ts, "POST", "/networks/create", `{"Name":"tc-net"}`, &n); r.StatusCode != 201 {
		t.Fatalf("network create: %d", r.StatusCode)
	}
	body := `{"Image":"fws/tiny:1","HostConfig":{"NetworkMode":"` + n.ID + `"},"NetworkingConfig":{"EndpointsConfig":{"` + n.ID + `":{"Aliases":["cache"]}}}}`
	var created map[string]any
	if r := call(t, ts, "POST", "/containers/create?name=redis", body, &created); r.StatusCode != 201 {
		t.Fatalf("create: %d %v", r.StatusCode, created)
	}
	var inspect container.InspectResponse
	call(t, ts, "GET", "/containers/redis/json", "", &inspect)
	if inspect.NetworkSettings == nil || len(inspect.NetworkSettings.Networks) != 1 {
		t.Fatalf("networks: %+v", inspect.NetworkSettings)
	}
	es := inspect.NetworkSettings.Networks["tc-net"]
	if es == nil || !slices.Contains(es.Aliases, "cache") || !slices.Contains(es.DNSNames, "cache") {
		t.Errorf("the endpoint on tc-net: %+v, want the alias cache", es)
	}
}
