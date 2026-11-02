//go:build e2e && darwin

package e2e

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
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

// waitedRun starts a detached container from image with args and waits for
// it to report healthy, returning its name.
func waitedRun(t *testing.T, name string, args ...string) {
	t.Helper()
	since := time.Now()
	mustDocker(t, append([]string{"run", "-d", "--name", name}, args...)...)
	t.Cleanup(func() { docker(t, "rm", "-f", "-v", name) })
	waitHealthy(t, name, since)
}

func TestPostgres18(t *testing.T) {
	const name = "fws-e2e-pg18"
	waitedRun(t, name, "-p", "127.0.0.1::5432", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app", "fws/postgres:18")
	port := strings.TrimSpace(mustDocker(t, "port", name, "5432"))
	_, p, _ := strings.Cut(port, ":")
	if got := query(t, p, "SELECT current_setting('server_version_num')::int / 10000"); len(got) != 1 || got[0] != "18" {
		t.Fatalf("server major version %v, want 18", got)
	}
}

// The language images run scripts by their #! lines, through the
// interpreter the image carries, and their package managers.
func TestLanguageImages(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("hello.py", "#!/usr/bin/env python3\nimport sys\nprint('python', sys.version_info[0], sys.version_info[1])\n")
	write("hello.mjs", "#!/usr/bin/env node\nconsole.log('node', process.versions.node.split('.')[0])\n")
	write("hello.ts", "const who: string = 'bun';\nconsole.log(who, Bun.version.split('.')[0]);\n")
	for _, c := range []struct {
		image string
		args  []string
		want  string
	}{
		{"fws/python:3.14", []string{"/app/hello.py"}, "python 3 14"},
		{"fws/python:3.14", []string{"python3", "-m", "ensurepip", "--version"}, "pip"},
		{"fws/node:26", []string{"/app/hello.mjs"}, "node 26"},
		{"fws/node:26", []string{"npm", "--version"}, "."},
		// Arguments are the program's to read, on the host: a path
		// relative to the image's working directory, /app, names the file.
		{"fws/bun:1", []string{"bun", "run", "hello.ts"}, "bun 1"},
	} {
		out, err := docker(t, append([]string{"run", "--rm", "-v", dir + ":/app", c.image}, c.args...)...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s %v: %q, %v; want %q", c.image, c.args, out, err, c.want)
		}
	}
	// pip installs a package from the internet through the stack's proxy
	// into the container.
	out, err := docker(t, "run", "--rm", "fws/python:3.14", "python3", "-c", `import importlib, subprocess, sys
subprocess.check_call([sys.executable, "-m", "ensurepip", "--upgrade"], stdout=subprocess.DEVNULL)
subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "--no-cache-dir", "six"])
importlib.invalidate_caches()
import six
print("six", six.__version__)`)
	if err != nil || !strings.Contains(out, "six 1.") {
		t.Errorf("pip install: %q, %v", out, err)
	}
}

func TestGitea(t *testing.T) {
	const name = "fws-e2e-gitea"
	waitedRun(t, name, "-p", "127.0.0.1::3000", "-e", "GITEA__security__INSTALL_LOCK=true", "fws/gitea:1")
	addr := "http://" + strings.TrimSpace(mustDocker(t, "port", name, "3000"))
	if body := httpGet(t, addr+"/api/v1/version"); !strings.Contains(body, `"version":"1.`) {
		t.Fatalf("version: %q", body)
	}
	mustDocker(t, "exec", name, "/entry", "admin", "user", "create", "--username", "e2e", "--password", "e2e-password-1", "--email", "e2e@example.com", "--admin", "--must-change-password=false")
	req, _ := http.NewRequest("POST", addr+"/api/v1/user/repos", strings.NewReader(`{"name":"repo","auto_init":true}`))
	req.SetBasicAuth("e2e", "e2e-password-1")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create a repository: %s %s", res.Status, body)
	}
	// auto_init commits through git, which the image carries.
	if body := httpGet(t, addr+"/api/v1/repos/e2e/repo/contents/README.md"); !strings.Contains(body, `"name":"README.md"`) {
		t.Fatalf("the initialised repository holds no README: %q", body)
	}
}

func TestDynamoDBLocal(t *testing.T) {
	const name = "fws-e2e-dynamodb"
	waitedRun(t, name, "-p", "127.0.0.1::8000", "fws/dynamodb-local:2")
	endpoint := "http://" + strings.TrimSpace(mustDocker(t, "port", name, "8000"))
	client := dynamodb.New(dynamodb.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	ctx := context.Background()
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("notes"),
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash}},
		BillingMode:          types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("notes"), Item: map[string]types.AttributeValue{
		"id": &types.AttributeValueMemberS{Value: "1"}, "body": &types.AttributeValueMemberS{Value: "kept"},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("notes"), Key: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := got.Item["body"].(*types.AttributeValueMemberS); !ok || b.Value != "kept" {
		t.Fatalf("item %v", got.Item)
	}
}

func TestRabbitMQ(t *testing.T) {
	const name = "fws-e2e-rabbitmq"
	waitedRun(t, name, "-p", "127.0.0.1::5672", "-e", "RABBITMQ_DEFAULT_USER=e2e", "-e", "RABBITMQ_DEFAULT_PASS=e2e-pass", "fws/rabbitmq:4")
	conn, err := amqp.Dial("amqp://e2e:e2e-pass@" + strings.TrimSpace(mustDocker(t, "port", name, "5672")) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclare("e2e", false, true, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.PublishWithContext(context.Background(), "", q.Name, false, false, amqp.Publishing{Body: []byte("through the broker")}); err != nil {
		t.Fatal(err)
	}
	msgs, err := ch.Consume(q.Name, "", true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-msgs:
		if string(m.Body) != "through the broker" {
			t.Fatalf("consumed %q", m.Body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no message arrived")
	}
}

// Playwright drives the image's Chromium headless shell.
func TestPlaywright(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "page.js"), []byte(`const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.setContent("<title>rendered in fws</title><p id=p>painted</p>");
  console.log("title", await page.title(), "text", await page.textContent("#p"));
  const shot = await page.screenshot();
  console.log("screenshot", shot.length > 1000);
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
`), 0o644)
	out, err := docker(t, "run", "--rm", "-v", dir+":/app", "fws/playwright:1.59", "node", "page.js")
	if err != nil || !strings.Contains(out, "title rendered in fws text painted") || !strings.Contains(out, "screenshot true") {
		t.Fatalf("playwright: %q, %v", out, err)
	}
}
