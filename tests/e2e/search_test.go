//go:build e2e && darwin

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmeili "github.com/testcontainers/testcontainers-go/modules/meilisearch"
	tcneo4j "github.com/testcontainers/testcontainers-go/modules/neo4j"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"
)

// meiliTask waits until Meilisearch has processed the task a request
// enqueued, polling the task's status as Meilisearch's clients do.
func meiliTask(t *testing.T, base, key, enqueued string) {
	t.Helper()
	var task struct{ TaskUID int }
	if err := json.Unmarshal([]byte(enqueued), &task); err != nil {
		t.Fatalf("the enqueued task: %v: %s", err, enqueued)
	}
	auth := map[string]string{"Authorization": "Bearer " + key}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		code, body := httpDo(t, "GET", fmt.Sprintf("%s/tasks/%d", base, task.TaskUID), "", auth)
		var st struct{ Status string }
		if code != 200 || json.Unmarshal([]byte(body), &st) != nil {
			t.Fatalf("task %d: %d %s", task.TaskUID, code, body)
		}
		switch st.Status {
		case "succeeded":
			return
		case "failed", "canceled":
			t.Fatalf("task %d: %s", task.TaskUID, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %d did not finish", task.TaskUID)
}

// Meilisearch indexes and searches documents behind the master key the
// module sets, and a dump it writes starts another instance through the
// module's dump import.
func TestMeilisearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	const key = "a-master-key-of-16-bytes"
	m, err := tcmeili.Run(ctx, "fws/meilisearch:1.43", tcmeili.WithMasterKey(key))
	testcontainers.CleanupContainer(t, m)
	if err != nil {
		t.Fatal(err)
	}
	base, err := m.Address(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
	if code, _ := httpDo(t, "GET", base+"/indexes", "", nil); code != 401 {
		t.Errorf("a request without the key: %d", code)
	}
	code, body := httpDo(t, "POST", base+"/indexes/notes/documents?primaryKey=id", `[{"id":1,"text":"kept in the index"},{"id":2,"text":"something else"}]`, auth)
	if code != 202 {
		t.Fatalf("add documents: %d %s", code, body)
	}
	meiliTask(t, base, key, body)
	code, body = httpDo(t, "POST", base+"/indexes/notes/search", `{"q":"kept"}`, auth)
	var found struct{ Hits []struct{ ID int } }
	if code != 200 || json.Unmarshal([]byte(body), &found) != nil || len(found.Hits) != 1 || found.Hits[0].ID != 1 {
		t.Fatalf("search: %d %s", code, body)
	}
	code, body = httpDo(t, "POST", base+"/dumps", "", auth)
	if code != 202 {
		t.Fatalf("create a dump: %d %s", code, body)
	}
	meiliTask(t, base, key, body)
	code, body = httpDo(t, "GET", base+"/tasks?types=dumpCreation", "", auth)
	var tasks struct {
		Results []struct{ Details struct{ DumpUID string } }
	}
	if code != 200 || json.Unmarshal([]byte(body), &tasks) != nil || len(tasks.Results) != 1 {
		t.Fatalf("the dump task: %d %s", code, body)
	}
	r, err := m.CopyFileFromContainer(ctx, "/meili_data/dumps/"+tasks.Results[0].Details.DumpUID+".dump")
	if err != nil {
		t.Fatal(err)
	}
	dump, _ := io.ReadAll(r)
	r.Close()
	path := filepath.Join(t.TempDir(), "notes.dump")
	if err := os.WriteFile(path, dump, 0o644); err != nil {
		t.Fatal(err)
	}
	restored, err := tcmeili.Run(ctx, "fws/meilisearch:1.43", tcmeili.WithMasterKey(key), tcmeili.WithDumpImport(path))
	testcontainers.CleanupContainer(t, restored)
	if err != nil {
		t.Fatal(err)
	}
	base, err = restored.Address(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := httpDo(t, "GET", base+"/indexes/notes/documents/1", "", auth); code != 200 || !strings.Contains(body, "kept in the index") {
		t.Fatalf("the imported document: %d %s", code, body)
	}
}

// Qdrant stores vectors and finds the nearest, through its REST port, and
// serves its dashboard; the module waits for its gRPC port too.
func TestQdrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	q, err := tcqdrant.Run(ctx, "fws/qdrant:1.17")
	testcontainers.CleanupContainer(t, q)
	if err != nil {
		t.Fatal(err)
	}
	base, err := q.RESTEndpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"Content-Type": "application/json"}
	if code, body := httpDo(t, "PUT", base+"/collections/notes", `{"vectors":{"size":2,"distance":"Dot"}}`, h); code != 200 {
		t.Fatalf("create the collection: %d %s", code, body)
	}
	if code, body := httpDo(t, "PUT", base+"/collections/notes/points?wait=true",
		`{"points":[{"id":1,"vector":[1,0],"payload":{"text":"kept"}},{"id":2,"vector":[0,1],"payload":{"text":"other"}}]}`, h); code != 200 {
		t.Fatalf("upsert: %d %s", code, body)
	}
	code, body := httpDo(t, "POST", base+"/collections/notes/points/query", `{"query":[1,0.1],"limit":1,"with_payload":true}`, h)
	var res struct {
		Result struct {
			Points []struct {
				ID      int
				Payload struct{ Text string }
			}
		}
	}
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res.Result.Points) != 1 || res.Result.Points[0].Payload.Text != "kept" {
		t.Fatalf("query: %d %s", code, body)
	}
	ui, err := q.WebUI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := httpDo(t, "GET", ui+"/", "", nil); code != 200 || !strings.Contains(body, "<html") {
		t.Errorf("the dashboard: %d %.200s", code, body)
	}
}

// Neo4j starts with the admin password, the APOC plugin from its own
// distribution and a setting the module passes as NEO4J_ variables, and
// answers Cypher through its Query API.
func TestNeo4j(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := tcneo4j.Run(ctx, "fws/neo4j:2026.02", tcneo4j.WithAdminPassword("letmein123"),
		tcneo4j.WithLabsPlugin(tcneo4j.Apoc), tcneo4j.WithNeo4jSetting("server.memory.heap.max_size", "512m"))
	testcontainers.CleanupContainer(t, n)
	if err != nil {
		t.Fatal(err)
	}
	logsOnFailure(t, n)
	base, err := n.PortEndpoint(ctx, "7474/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	query := `{"statement":"CREATE (n:Note {text: $t}) RETURN apoc.version() AS v, n.text AS t","parameters":{"t":"kept"}}`
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	h["Authorization"] = "Basic bmVvNGo6d3Jvbmc=" // neo4j:wrong
	if code, _ := httpDo(t, "POST", base+"/db/neo4j/query/v2", query, h); code != 401 {
		t.Errorf("a wrong password: %d", code)
	}
	h["Authorization"] = "Basic bmVvNGo6bGV0bWVpbjEyMw==" // neo4j:letmein123
	code, body := httpDo(t, "POST", base+"/db/neo4j/query/v2", query, h)
	var res struct {
		Data struct{ Values [][]string }
	}
	if code != 202 && code != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res.Data.Values) != 1 ||
		res.Data.Values[0][0] != "2026.02.2" || res.Data.Values[0][1] != "kept" {
		t.Fatalf("the query: %d %s", code, body)
	}
	r, err := n.CopyFileFromContainer(ctx, "/var/lib/neo4j/conf/neo4j.conf")
	if err != nil {
		t.Fatal(err)
	}
	conf, _ := io.ReadAll(r)
	r.Close()
	for _, want := range []string{"\nserver.memory.heap.max_size=512m\n", "\ndbms.security.procedures.unrestricted=apoc.*\n", "\nserver.default_listen_address=0.0.0.0\n"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("neo4j.conf lacks %q", strings.TrimSpace(want))
		}
	}
}
