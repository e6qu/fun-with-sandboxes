//go:build e2e && darwin

package e2e

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"os/exec"
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
	"github.com/testcontainers/testcontainers-go"
	tcdynamodb "github.com/testcontainers/testcontainers-go/modules/dynamodb"
	tcnginx "github.com/testcontainers/testcontainers-go/modules/nginx"
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

	// Content from a bind mount, and a server from a template that listens
	// on the container's port and names a variable of its own.
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("served from the host\n"), 0o644)
	templates := t.TempDir()
	os.WriteFile(filepath.Join(templates, "default.conf.template"), []byte(`server {
    listen 80;
    location /hello { return 200 "templated ${GREETING}\n"; }
    location / { root /usr/share/nginx/html; }
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
	mustDocker(t, "restart", name)
	waitHealthy(t, name, startedAt(t, name))
	addr = strings.TrimSpace(mustDocker(t, "port", name, "3306"))
	again := open("app:apppw@tcp(" + addr + ")/app")
	defer again.Close()
	var body string
	if err := again.QueryRow("SELECT body FROM notes").Scan(&body); err != nil || body != "kept" {
		t.Fatalf("after a restart the table holds %q, %v", body, err)
	}
	// docker logs writes the container's standard error, where MariaDB logs, to its own.
	logs, err := exec.Command("docker", "logs", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs %s: %v\n%s", name, err, logs)
	}
	if !strings.Contains(string(logs), "skipping initialization") {
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
	type check struct {
		image string
		args  []string
		want  string
	}
	checks := []check{
		// Arguments are the program's to read, on the host: a path
		// relative to the image's working directory, /app, names the file.
		{"fws/bun:1", []string{"bun", "run", "hello.ts"}, "bun 1"},
	}
	// Every release the catalog carries runs as itself, with its package
	// manager.
	for _, v := range []string{"3.11", "3.12", "3.13", "3.14", "3.15"} {
		checks = append(checks,
			check{"fws/python:" + v, []string{"/app/hello.py"}, "python " + strings.Replace(v, ".", " ", 1)},
			check{"fws/python:" + v, []string{"python3", "-m", "ensurepip", "--version"}, "pip"})
	}
	for _, v := range []string{"22", "24", "26"} {
		checks = append(checks,
			check{"fws/node:" + v, []string{"/app/hello.mjs"}, "node " + v},
			check{"fws/node:" + v, []string{"npm", "--version"}, "."})
	}
	for _, c := range checks {
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

// fws/busybox runs scripts' everyday commands under its shell, and
// fws/base adds bash, curl over TLS with its CA bundle, git and jq, and
// builds an image FROM it whose shell-form RUN steps use them.
func TestShellImages(t *testing.T) {
	for _, c := range []struct {
		image  string
		script string
		want   string
	}{
		{"fws/busybox:1", `echo hello | sed s/hello/world/ | grep world && printf 'b\na\n' | sort | head -n 1 &&
echo x | awk '{print toupper($0)}' && mkdir -p /tmp/d && echo hi > /tmp/d/f && tar -C /tmp -czf /tmp/d.tgz d &&
tar -tzf /tmp/d.tgz | grep -c d/f && find /tmp/d -name f && diff /tmp/d/f /tmp/d/f && which wget >/dev/null && echo done`,
			"world\na\nX\n1\n/tmp/d/f\ndone\n"},
		{"fws/busybox:1", `busybox echo by name && busybox --list | grep -x sed && busybox nonexistent; echo $?`, "by name\nsed\n127\n"},
		// Its tar unpacks a zip from a pipe, modes and all, as the Dev
		// Containers extension unpacks VS Code's server; which finds it on
		// PATH, and PATH and HOME name the container's directories.
		{"fws/busybox:1", `mkdir -p /tmp/z/bin && printf '#!/bin/sh\necho ran\n' > /tmp/z/bin/x && chmod 755 /tmp/z/bin/x &&
bsdtar --format zip -cf - -C /tmp z | tar --no-same-owner -xz -C /root && /root/z/bin/x && which tar && echo "$PATH" && echo "$HOME"`,
			"ran\n/bin/tar\n/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n/root\n"},
		{"fws/base:1", `bash -c 'echo "bash ${BASH_VERSINFO[0]}"' && jq -n '{a: 41} | .a + 1' &&
curl -sS -o /dev/null -w '%{http_code}\n' https://github.com/ &&
git init -q /tmp/r && git -C /tmp/r -c user.email=e2e@example.com -c user.name=e2e commit -q --allow-empty -m first &&
git -C /tmp/r log --format=%s`, "bash 5\n42\n200\nfirst\n"},
	} {
		out, err := docker(t, "run", "--rm", c.image, "sh", "-c", c.script)
		if err != nil || out != c.want {
			t.Errorf("%s: %q, %v; want %q", c.image, out, err, c.want)
		}
	}
	for image, cmd := range map[string]string{"fws/busybox:1": `["sh"]`, "fws/base:1": `["bash"]`} {
		if out := strings.TrimSpace(mustDocker(t, "image", "inspect", "--format", "{{json .Config.Cmd}}", image)); out != cmd {
			t.Errorf("%s's command: %s, want %s", image, out, cmd)
		}
	}

	dir := t.TempDir()
	dockerfile := "FROM fws/base:1\nRUN echo built > /built.txt && jq -n '\"jq ran\"' >> /built.txt\nCMD cat /built.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("docker", "build", "-t", "e2e/from-base:1", dir)
	build.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build FROM fws/base:1: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/from-base:1")
	if out := mustDocker(t, "run", "--rm", "e2e/from-base:1"); out != "built\n\"jq ran\"\n" {
		t.Errorf("the image built FROM fws/base:1: %q", out)
	}
}

// toolchainRun runs script under sh in a container of image, with dir at
// /app, and fails the test unless the output holds every one of want.
func toolchainRun(t *testing.T, image, dir, script string, want ...string) {
	t.Helper()
	out, err := docker(t, "run", "--rm", "-v", dir+":/app", "-w", "/app", image, "sh", "-c", "set -e\n"+script)
	for _, w := range want {
		if err != nil || !strings.Contains(out, w) {
			t.Errorf("%s: %q, %v; want %q", image, out, err, w)
			return
		}
	}
}

// The Node.js images build what the official images build: npm installs a
// package whose native addon node-gyp compiles with the image's python3,
// make, C++ compiler and the macOS SDK against the image's Node.js
// headers; corepack's pnpm installs a package from the registry through
// the egress proxy, which corepack reaches through NODE_USE_ENV_PROXY; and
// Yarn 1 is there, as in node:22 and node:24.
func TestNodeToolchain(t *testing.T) {
	for _, v := range []string{"22", "24", "26"} {
		t.Run(v, func(t *testing.T) {
			image := "fws/node:" + v
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{
				"package.json":       `{"name": "app", "version": "1.0.0", "private": true, "dependencies": {"hello-addon": "file:./addon"}}`,
				"addon/package.json": `{"name": "hello-addon", "version": "1.0.0", "main": "index.js"}`,
				"addon/index.js":     "module.exports = require('./build/Release/hello.node');\n",
				"addon/binding.gyp":  `{"targets": [{"target_name": "hello", "sources": ["hello.cc"]}]}`,
				"addon/hello.cc": `#include <node_api.h>
#include <string>

static napi_value Hello(napi_env env, napi_callback_info) {
  std::string s = "native " + std::to_string(6 * 7);
  napi_value v;
  napi_create_string_utf8(env, s.c_str(), s.size(), &v);
  return v;
}

static napi_value Init(napi_env env, napi_value exports) {
  napi_value fn;
  napi_create_function(env, "hello", NAPI_AUTO_LENGTH, Hello, nullptr, &fn);
  napi_set_named_property(env, exports, "hello", fn);
  return exports;
}

NAPI_MODULE(NODE_GYP_MODULE_NAME, Init)
`,
			})
			// --install-links installs the addon as a copy, as from the
			// registry, so npm runs its default install script, node-gyp
			// rebuild.
			toolchainRun(t, image, dir, `npx --version >/dev/null
cc --version | head -n 1 && python3 --version && make --version | head -n 1
npm install --install-links --no-audit --no-fund --loglevel=error
node -e 'console.log(require("hello-addon").hello())'`, "clang version", "Python 3", "GNU Make", "native 42")
			if v == "26" {
				// Node.js 25 and its official images dropped corepack and Yarn.
				return
			}
			toolchainRun(t, image, dir, `yarn --version
corepack enable pnpm
mkdir -p /tmp/p && cd /tmp/p
echo '{"name": "p", "version": "1.0.0", "private": true, "dependencies": {"is-number": "7.0.0"}}' > package.json
pnpm install --reporter=silent
node -e 'console.log("pnpm", require("is-number")(5))'`, "1.22.22", "pnpm true")
		})
	}
}

// Bun installs from the registry through the egress proxy, runs the
// project's postinstall script with /bin/sh, where node is bun, runs a
// dependency's bin with bunx, and runs bun test.
func TestBunToolchain(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"package.json": `{"name": "b", "version": "1.0.0", "private": true,
 "scripts": {"postinstall": "echo postinstall ran > postinstall.txt && node -e 'console.log(1 + 1)' >> postinstall.txt"},
 "dependencies": {"semver": "7.6.3"}}`,
		"semver.test.ts": `import { expect, test } from "bun:test";
import semver from "semver";

test("semver orders versions", () => {
  expect(semver.gt("1.2.3", "1.2.2")).toBe(true);
});
`,
	})
	toolchainRun(t, "fws/bun:1", dir, `bun install
cat postinstall.txt
bunx semver 1.2.3
bun test 2>&1`, "postinstall ran\n2\n", "1.2.3", "1 pass")
}

// fws/rust builds and runs a crate with cargo, formats and lints it with
// rustfmt and clippy, builds a crate whose build script compiles C with the
// cc crate from crates.io, and builds a Dockerfile FROM it whose binary a
// second stage copies into fws/base.
func TestRustToolchain(t *testing.T) {
	const image = "fws/rust:1.100"
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"Cargo.toml": `[package]
name = "addc"
version = "0.1.0"
edition = "2024"

[build-dependencies]
cc = "1"
`,
		"build.rs": `fn main() {
    cc::Build::new().file("src/add.c").compile("add");
}
`,
		"src/add.c": "int add(int a, int b) { return a + b; }\n",
		"src/main.rs": `unsafe extern "C" {
    fn add(a: i32, b: i32) -> i32;
}

fn main() {
    println!("c says {}", unsafe { add(40, 2) });
}
`,
	})
	toolchainRun(t, image, dir, `rustc --version && cargo --version && cargo fmt --version && cargo clippy --version
cd /tmp && cargo new --vcs none --quiet hello && cd hello
cargo build --release --quiet && ./target/release/hello
cargo fmt --check && cargo clippy --quiet --release -- -D warnings
cd /app && cargo run --release --quiet`, "rustc 1.100.0", "Hello, world!", "c says 42")

	ctx := t.TempDir()
	writeTree(t, ctx, map[string]string{
		"Cargo.toml":  "[package]\nname = \"hello\"\nversion = \"0.1.0\"\nedition = \"2024\"\n",
		"src/main.rs": "fn main() {\n    println!(\"built FROM {}\", env!(\"CARGO_PKG_NAME\"));\n}\n",
		"Dockerfile": `FROM ` + image + ` AS build
WORKDIR /src
COPY . .
RUN cargo build --release
FROM fws/base:1
COPY --from=build /src/target/release/hello /usr/local/bin/hello
CMD ["hello"]
`,
	})
	if out, err := exec.Command("docker", "buildx", "build", "--load", "-t", "e2e/rust-hello:1", ctx).CombinedOutput(); err != nil {
		t.Fatalf("docker build FROM %s: %v\n%s", image, err, out)
	}
	defer docker(t, "rmi", "e2e/rust-hello:1")
	if out := mustDocker(t, "run", "--rm", "e2e/rust-hello:1"); out != "built FROM hello\n" {
		t.Errorf("the binary built FROM %s, run in fws/base:1: %q", image, out)
	}
}

func TestGitea(t *testing.T) {
	const name = "fws-e2e-gitea"
	waitedRun(t, name, "-p", "127.0.0.1::3000", "-e", "GITEA__security__INSTALL_LOCK=true", "fws/gitea:28")
	addr := "http://" + strings.TrimSpace(mustDocker(t, "port", name, "3000"))
	if body := httpGet(t, addr+"/api/v1/version"); !strings.Contains(body, `"version":"28.`) {
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

// Testcontainers' DynamoDB module runs fws/dynamodb-local as it runs
// amazon/dynamodb-local: its entrypoint is java with DynamoDB Local's
// native libraries, and its command the jar, both named relative to the
// image's working directory. WithSharedDB reuses the container by name,
// so a second Run finds the first's container and its table.
func TestDynamoDBModule(t *testing.T) {
	ctx := context.Background()
	c, err := tcdynamodb.Run(ctx, "fws/dynamodb-local:2", tcdynamodb.WithSharedDB(), tcdynamodb.WithDisableTelemetry())
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	clientOf := func(c *tcdynamodb.DynamoDBContainer) *dynamodb.Client {
		t.Helper()
		hostPort, err := c.ConnectionString(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return dynamodb.New(dynamodb.Options{
			Region: "us-east-1", BaseEndpoint: aws.String("http://" + hostPort),
			Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		})
	}
	client := clientOf(c)
	if _, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("notes"),
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash}},
		BillingMode:          types.BillingModePayPerRequest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("notes"), Item: map[string]types.AttributeValue{
		"id": &types.AttributeValueMemberS{Value: "1"}, "body": &types.AttributeValueMemberS{Value: "shared"},
	}}); err != nil {
		t.Fatal(err)
	}
	again, err := tcdynamodb.Run(ctx, "fws/dynamodb-local:2", tcdynamodb.WithSharedDB(), tcdynamodb.WithDisableTelemetry())
	if err != nil {
		t.Fatal(err)
	}
	if again.GetContainerID() != c.GetContainerID() {
		testcontainers.CleanupContainer(t, again)
		t.Fatalf("WithSharedDB made a second container, %s beside %s", again.GetContainerID(), c.GetContainerID())
	}
	got, err := clientOf(again).GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("notes"), Key: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := got.Item["body"].(*types.AttributeValueMemberS); !ok || b.Value != "shared" {
		t.Fatalf("the reused container's item %v", got.Item)
	}
}

// Testcontainers' nginx module runs fws/nginx with a server of the
// test's in conf.d, WithCustomConfig, and with a main configuration of
// the test's in place of the image's, WithConfigFile, as it runs the
// official image.
func TestNginxModule(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	get := func(t *testing.T, c *tcnginx.Container) string {
		t.Helper()
		endpoint, err := c.HTTPEndpoint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return httpGet(t, endpoint+"/")
	}
	t.Run("custom config", func(t *testing.T) {
		snippet := filepath.Join(dir, "default.conf")
		os.WriteFile(snippet, []byte("server {\n    listen 80;\n    location / { return 200 \"from the module's server\\n\"; }\n}\n"), 0o644)
		c, err := tcnginx.Run(ctx, "fws/nginx:1", tcnginx.WithCustomConfig(snippet))
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		if body := get(t, c); body != "from the module's server\n" {
			t.Errorf("the server of the module's conf.d answered %q", body)
		}
	})
	t.Run("config file", func(t *testing.T) {
		main := filepath.Join(dir, "nginx.conf")
		os.WriteFile(main, []byte(`pid /tmp/nginx.pid;
error_log stderr;
events {}
http {
    access_log off;
    server {
        listen 80;
        location / { return 200 "from the module's nginx.conf\n"; }
    }
}
`), 0o644)
		c, err := tcnginx.Run(ctx, "fws/nginx:1", tcnginx.WithConfigFile(main))
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		if body := get(t, c); body != "from the module's nginx.conf\n" {
			t.Errorf("nginx with the module's nginx.conf answered %q", body)
		}
	})
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
	// A durable queue: RabbitMQ 4.4 refuses transient non-exclusive ones
	// by default (the deprecated feature transient_nonexcl_queues).
	q, err := ch.QueueDeclare("e2e", true, true, false, false, nil)
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

// Playwright's library drives each browser the image carries, from a
// script that requires playwright through the image's NODE_PATH: Chromium's
// headless shell, Chromium itself in its new headless mode, and Firefox.
func TestPlaywright(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "page.js"), []byte(`const { chromium, firefox } = require("playwright");
(async () => {
  for (const [name, type, options] of [["chromium", chromium, {}], ["firefox", firefox, {}]]) {
    const browser = await type.launch(options);
    const page = await browser.newPage();
    await page.setContent("<title>rendered in fws</title><p id=p>painted</p>");
    console.log(name, "title", await page.title(), "text", await page.textContent("#p"));
    const shot = await page.screenshot();
    console.log(name, "screenshot", shot.length > 1000);
    await browser.close();
  }
})().catch((e) => { console.error(e); process.exit(1); });
`), 0o644)
	out, err := docker(t, "run", "--rm", "-v", dir+":/app", "fws/playwright:1.65", "node", "page.js")
	if err != nil {
		t.Fatalf("playwright: %q, %v", out, err)
	}
	for _, name := range []string{"chromium", "firefox"} {
		if !strings.Contains(out, name+" title rendered in fws text painted") || !strings.Contains(out, name+" screenshot true") {
			t.Errorf("playwright with %s: %q", name, out)
		}
	}
}

// `npx playwright test` runs the image's @playwright/test over a project
// that mounts only its config and specs, as Microsoft's image runs one:
// each browser project launches its browser headless, renders a page,
// screenshots it and records a video of it through the image's ffmpeg.
func TestPlaywrightTest(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "playwright.config.ts"), []byte(`import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  reporter: "list",
  use: { video: "on" },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "firefox", use: { ...devices["Desktop Firefox"] } },
  ],
});
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	os.WriteFile(filepath.Join(dir, "tests", "page.spec.ts"), []byte(`import { test, expect } from "@playwright/test";

test("renders and screenshots a page", async ({ page, browserName }, testInfo) => {
  await page.setContent("<title>rendered in fws</title><p id=p style='font-size:40px'>painted by " + browserName + "</p>");
  await expect(page).toHaveTitle("rendered in fws");
  await expect(page.locator("#p")).toHaveText("painted by " + browserName);
  const shot = await page.screenshot({ path: testInfo.outputPath("page.png") });
  expect(shot.length).toBeGreaterThan(1000);
});
`), 0o644)
	out, err := docker(t, "run", "--rm", "-v", dir+":/app", "fws/playwright:1.65", "npx", "playwright", "test")
	if err != nil || !strings.Contains(out, "2 passed") {
		t.Fatalf("npx playwright test: %q, %v", out, err)
	}
	// Each test's output directory ends with its project's name.
	for _, project := range []string{"chromium", "firefox"} {
		for _, f := range []string{"page.png", "video.webm"} {
			found, _ := filepath.Glob(filepath.Join(dir, "test-results", "*-"+project, f))
			if len(found) != 1 {
				t.Errorf("%s: %s %v", project, f, found)
				continue
			}
			if fi, err := os.Stat(found[0]); err != nil || fi.Size() == 0 {
				t.Errorf("%s: %s empty: %v", project, found[0], err)
			}
		}
	}
}

// A catalog server listens on its container port, so its own client in the
// container reaches it with no port or socket named, as in the official
// image, and the server reports the container's port.
func TestCatalogClientsOnDefaultPorts(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		const name = "fws-e2e-default-pg"
		waitedRun(t, name, "-e", "POSTGRES_PASSWORD=secret", "fws/postgres:16")
		out := strings.TrimSpace(mustDocker(t, "exec", name, "psql", "-U", "postgres", "-tA", "-c", "SHOW port", "-c", "SHOW listen_addresses"))
		if out != "5432\n*" {
			t.Fatalf("psql on the default socket: port and listen addresses %q, want 5432 and *", out)
		}
	})
	t.Run("redis", func(t *testing.T) {
		const name = "fws-e2e-default-redis"
		waitedRun(t, name, "fws/redis:8")
		if out := strings.TrimSpace(mustDocker(t, "exec", name, "redis-cli", "ping")); out != "PONG" {
			t.Fatalf("redis-cli ping: %q", out)
		}
		if out := strings.Fields(mustDocker(t, "exec", name, "redis-cli", "config", "get", "port")); len(out) != 2 || out[1] != "6379" {
			t.Fatalf("redis-cli config get port: %q", out)
		}
	})
}
