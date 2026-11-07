//go:build e2e && darwin

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcconsul "github.com/testcontainers/testcontainers-go/modules/consul"
	tcetcd "github.com/testcontainers/testcontainers-go/modules/etcd"
	tcmongodb "github.com/testcontainers/testcontainers-go/modules/mongodb"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// httpDo sends a request and returns the response's status and body.
func httpDo(t *testing.T, method, url, body string, header map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// etcd serves its key-value API on the container's client port, alone and
// as a cluster of three, whose members, each in a stack of its own, reach
// one another by name on the network they share. The module waits for
// nothing, so each node waits for its client port to listen.
func TestEtcd(t *testing.T) {
	ctx := context.Background()
	listening := testcontainers.WithWaitStrategy(wait.ForListeningPort("2379/tcp"))
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	put := func(endpoint, key, value string) {
		t.Helper()
		if code, body := httpDo(t, "POST", endpoint+"/v3/kv/put", `{"key":"`+b64(key)+`","value":"`+b64(value)+`"}`, nil); code != 200 {
			t.Fatalf("put %s at %s: %d %s", key, endpoint, code, body)
		}
	}
	get := func(endpoint, key string) string {
		t.Helper()
		code, body := httpDo(t, "POST", endpoint+"/v3/kv/range", `{"key":"`+b64(key)+`"}`, nil)
		var r struct{ Kvs []struct{ Value string } }
		if code != 200 || json.Unmarshal([]byte(body), &r) != nil || len(r.Kvs) != 1 {
			t.Fatalf("range %s at %s: %d %s", key, endpoint, code, body)
		}
		v, _ := base64.StdEncoding.DecodeString(r.Kvs[0].Value)
		return string(v)
	}

	single, err := tcetcd.Run(ctx, "fws/etcd:3.6", listening)
	testcontainers.CleanupContainer(t, single)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := single.ClientEndpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	put(endpoint, "note", "kept")
	if v := get(endpoint, "note"); v != "kept" {
		t.Errorf("a single node read %q", v)
	}

	cluster, err := tcetcd.Run(ctx, "fws/etcd:3.6", listening, tcetcd.WithNodes("etcd-1", "etcd-2", "etcd-3"))
	testcontainers.CleanupContainer(t, cluster)
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := cluster.ClientEndpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 3 {
		t.Fatalf("the cluster's endpoints: %v", endpoints)
	}
	code, body := httpDo(t, "POST", endpoints[0]+"/v3/cluster/member/list", "{}", nil)
	var members struct{ Members []struct{ Name string } }
	if code != 200 || json.Unmarshal([]byte(body), &members) != nil || len(members.Members) != 3 {
		t.Fatalf("the cluster's members: %d %s", code, body)
	}
	put(endpoints[0], "replicated", "everywhere")
	for _, e := range endpoints[1:] {
		if v := get(e, "replicated"); v != "everywhere" {
			t.Errorf("%s read %q", e, v)
		}
	}
}

// Consul's development agent serves its HTTP API on the container's port,
// with the configuration Testcontainers writes from CONSUL_LOCAL_CONFIG.
func TestConsul(t *testing.T) {
	ctx := context.Background()
	c, err := tcconsul.Run(ctx, "fws/consul:2.1", tcconsul.WithConfigString(`{"node_name": "fws-node"}`))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := c.ApiEndpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + endpoint
	if code, body := httpDo(t, "PUT", base+"/v1/kv/note", "kept", nil); code != 200 || strings.TrimSpace(body) != "true" {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, body := httpDo(t, "GET", base+"/v1/kv/note?raw", "", nil); code != 200 || body != "kept" {
		t.Errorf("get: %d %q", code, body)
	}
	code, body := httpDo(t, "GET", base+"/v1/agent/self", "", nil)
	var self struct{ Config struct{ NodeName string } }
	if code != 200 || json.Unmarshal([]byte(body), &self) != nil || self.Config.NodeName != "fws-node" {
		t.Errorf("the agent: %d %s", code, body)
	}
}

// Vault's development server takes the root token Testcontainers names,
// and the commands Testcontainers runs through the image's vault set it
// up before it reads as ready.
func TestVault(t *testing.T) {
	ctx := context.Background()
	v, err := tcvault.Run(ctx, "fws/vault:2.0", tcvault.WithToken("root-token"),
		tcvault.WithInitCommand("secrets enable transit", "write -f transit/keys/note-key", "kv put secret/note body=kept"))
	testcontainers.CleanupContainer(t, v)
	if err != nil {
		t.Fatal(err)
	}
	base, err := v.HttpHostAddress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token := map[string]string{"X-Vault-Token": "root-token"}
	code, body := httpDo(t, "GET", base+"/v1/secret/data/note", "", token)
	var secret struct {
		Data struct{ Data map[string]string }
	}
	if code != 200 || json.Unmarshal([]byte(body), &secret) != nil || secret.Data.Data["body"] != "kept" {
		t.Errorf("the secret: %d %s", code, body)
	}
	if code, body := httpDo(t, "GET", base+"/v1/transit/keys/note-key", "", token); code != 200 {
		t.Errorf("the transit key: %d %s", code, body)
	}
	if code, _ := httpDo(t, "GET", base+"/v1/secret/data/note", "", map[string]string{"X-Vault-Token": "wrong"}); code != http.StatusForbidden {
		t.Errorf("a wrong token read the secret: %d", code)
	}
}

// MongoDB starts as Testcontainers runs it: alone; with a root user the
// entrypoint creates, after which the server requires authentication; and
// as a one-member replica set initiated at the container's address, with
// and without a root user.
func TestMongoDB(t *testing.T) {
	ctx := context.Background()
	auth := []testcontainers.ContainerCustomizer{tcmongodb.WithUsername("root"), tcmongodb.WithPassword("rootpw")}
	for _, c := range []struct {
		name    string
		opts    []testcontainers.ContainerCustomizer
		auth    bool
		replSet string
	}{
		{name: "standalone"},
		{name: "root user", opts: auth, auth: true},
		{name: "replica set", opts: []testcontainers.ContainerCustomizer{tcmongodb.WithReplicaSet("rs0")}, replSet: "rs0"},
		{name: "replica set with a root user", opts: append(append([]testcontainers.ContainerCustomizer{}, auth...), tcmongodb.WithReplicaSet("rs1")), auth: true, replSet: "rs1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, err := tcmongodb.Run(ctx, "fws/mongo:8.2", c.opts...)
			testcontainers.CleanupContainer(t, m)
			if err != nil {
				t.Fatal(err)
			}
			uri, err := m.ConnectionString(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// A replica set's member is at the container's address, which
			// the host does not route, so the client connects to the
			// mapped port directly.
			client, err := mongo.Connect(options.Client().ApplyURI(uri).SetDirect(true).SetTimeout(20 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Disconnect(ctx)
			notes := client.Database("app").Collection("notes")
			if _, err := notes.InsertOne(ctx, bson.D{{Key: "body", Value: "kept"}}); err != nil {
				t.Fatal(err)
			}
			var got struct{ Body string }
			if err := notes.FindOne(ctx, bson.D{}).Decode(&got); err != nil || got.Body != "kept" {
				t.Fatalf("read %+v, %v", got, err)
			}
			var hello struct{ SetName string }
			if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || hello.SetName != c.replSet {
				t.Errorf("hello: set %q, %v; want %q", hello.SetName, err, c.replSet)
			}
			if c.auth {
				endpoint, err := m.PortEndpoint(ctx, "27017/tcp", "")
				if err != nil {
					t.Fatal(err)
				}
				anon, err := mongo.Connect(options.Client().ApplyURI("mongodb://" + endpoint).SetDirect(true).SetTimeout(20 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				defer anon.Disconnect(ctx)
				if _, err := anon.Database("app").Collection("notes").InsertOne(ctx, bson.D{{Key: "body", Value: "denied"}}); err == nil {
					t.Error("a client without credentials wrote")
				}
			}
		})
	}
}
