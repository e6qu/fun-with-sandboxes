//go:build e2e && darwin

package e2e

import (
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func httpGet(t *testing.T, url string) string {
	t.Helper()
	c := http.Client{Timeout: 10 * time.Second}
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestNginx(t *testing.T) {
	since := time.Now().Add(-time.Second)
	const plain = "fws-e2e-nginx"
	mustDocker(t, "run", "-d", "--name", plain, "-p", "127.0.0.1::80", "fws/nginx:1")
	defer docker(t, "rm", "-f", plain)
	waitHealthy(t, plain, since)
	if body := httpGet(t, "http://"+strings.TrimSpace(mustDocker(t, "port", plain, "80"))+"/"); !strings.Contains(body, "Welcome to nginx") {
		t.Fatalf("the default page reads %q", body)
	}

	// Content from a bind mount, and a server from a template that names
	// the port the engine assigned and a variable of its own.
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("served from the host\n"), 0o644)
	templates := t.TempDir()
	os.WriteFile(filepath.Join(templates, "default.conf.template"), []byte(`server {
    listen 127.0.0.1:${SANDBOX_PORT_80};
    location /hello { return 200 "templated ${GREETING}\n"; }
    location / { root ${SANDBOX_ROOT}/usr/share/nginx/html; }
}
`), 0o644)
	const custom = "fws-e2e-nginx-templated"
	since = time.Now().Add(-time.Second)
	mustDocker(t, "run", "-d", "--name", custom, "-p", "127.0.0.1::80", "-e", "GREETING=hi",
		"-v", site+":/usr/share/nginx/html:ro", "-v", templates+":/etc/nginx/templates:ro", "fws/nginx:1")
	defer docker(t, "rm", "-f", custom)
	waitHealthy(t, custom, since)
	addr := "http://" + strings.TrimSpace(mustDocker(t, "port", custom, "80"))
	if body := httpGet(t, addr+"/"); body != "served from the host\n" {
		t.Fatalf("the bind-mounted site reads %q", body)
	}
	if body := httpGet(t, addr+"/hello"); body != "templated hi\n" {
		t.Fatalf("the templated server answered %q", body)
	}
}

func TestMariaDB(t *testing.T) {
	since := time.Now().Add(-time.Second)
	const name = "fws-e2e-mariadb"
	mustDocker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::3306",
		"-e", "MARIADB_ROOT_PASSWORD=rootpw", "-e", "MARIADB_DATABASE=app", "-e", "MARIADB_USER=app", "-e", "MARIADB_PASSWORD=apppw",
		"fws/mariadb:11")
	defer docker(t, "rm", "-f", "-v", name)
	waitHealthy(t, name, since)
	addr := strings.TrimSpace(mustDocker(t, "port", name, "3306"))
	open := func(dsn string) *sql.DB {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	app := open("app:apppw@tcp(" + addr + ")/app")
	defer app.Close()
	for _, q := range []string{"CREATE TABLE notes (body TEXT)", "INSERT INTO notes VALUES ('kept')"} {
		if _, err := app.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	root := open("root:rootpw@tcp(" + addr + ")/")
	defer root.Close()
	var version string
	if err := root.QueryRow("SELECT VERSION()").Scan(&version); err != nil || !strings.Contains(version, "MariaDB") {
		t.Fatalf("root sees version %q, %v", version, err)
	}
	if _, err := open("root:wrong@tcp(" + addr + ")/").Exec("SELECT 1"); err == nil {
		t.Fatal("a wrong root password was accepted")
	}

	// The data outlives a restart, which skips initialisation.
	since = time.Now()
	mustDocker(t, "restart", name)
	waitHealthy(t, name, since)
	addr = strings.TrimSpace(mustDocker(t, "port", name, "3306"))
	again := open("app:apppw@tcp(" + addr + ")/app")
	defer again.Close()
	var body string
	if err := again.QueryRow("SELECT body FROM notes").Scan(&body); err != nil || body != "kept" {
		t.Fatalf("after a restart the table holds %q, %v", body, err)
	}
	if logs := mustDocker(t, "logs", name); !strings.Contains(logs, "skipping initialization") {
		t.Errorf("the restarted server initialised again:\n%s", logs)
	}
}
