//go:build e2e && darwin

package e2e

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcmariadb "github.com/testcontainers/testcontainers-go/modules/mariadb"
	tcmemcached "github.com/testcontainers/testcontainers-go/modules/memcached"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
)

// MySQL initialises from the environment and init scripts, reads its
// configuration from /etc/mysql/conf.d, and reports its container port.
func TestMySQL(t *testing.T) {
	since := time.Now().Add(-time.Second)
	const name = "fws-e2e-mysql"
	conf, initdb := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(conf, "e2e.cnf"), []byte("[mysqld]\nmax_connections=77\n"), 0o644)
	os.WriteFile(filepath.Join(initdb, "01-notes.sql"), []byte("CREATE TABLE notes (body TEXT);\nINSERT INTO notes VALUES ('from the init script');\n"), 0o644)
	mustDocker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::3306",
		"-e", "MYSQL_ROOT_PASSWORD=rootpw", "-e", "MYSQL_DATABASE=app", "-e", "MYSQL_USER=app", "-e", "MYSQL_PASSWORD=apppw",
		"-v", conf+":/etc/mysql/conf.d:ro", "-v", initdb+":/docker-entrypoint-initdb.d:ro", "fws/mysql:8.4")
	defer docker(t, "rm", "-f", "-v", name)
	waitHealthy(t, name, since)
	if logs := containerLogs(t, name); !strings.Contains(logs, "port: 3306") {
		t.Errorf("the server's log names no container port 3306:\n%s", logs)
	}
	addr := strings.TrimSpace(mustDocker(t, "port", name, "3306"))
	open := func(dsn string) *sql.DB {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	app := open("app:apppw@tcp(" + addr + ")/app")
	var body string
	if err := app.QueryRow("SELECT body FROM notes").Scan(&body); err != nil || body != "from the init script" {
		t.Fatalf("the init script's row reads %q, %v", body, err)
	}
	root := open("root:rootpw@tcp(" + addr + ")/")
	var version string
	var maxConns int
	if err := root.QueryRow("SELECT VERSION(), @@max_connections").Scan(&version, &maxConns); err != nil || !strings.HasPrefix(version, "8.4.") || maxConns != 77 {
		t.Fatalf("root sees version %q and max_connections %d, %v", version, maxConns, err)
	}
	if _, err := open("root:wrong@tcp(" + addr + ")/").Exec("SELECT 1"); err == nil {
		t.Fatal("a wrong root password was accepted")
	}
	// The client in the image reaches the server on its socket, as
	// /etc/my.cnf names it.
	if out := mustDocker(t, "exec", name, "mysql", "-uroot", "-prootpw", "-N", "-e", "SELECT COUNT(*) FROM app.notes"); strings.TrimSpace(lastLine(out)) != "1" {
		t.Errorf("the image's client printed %q", out)
	}
	// Without a password option the server refuses to initialise.
	if _, err := docker(t, "run", "--rm", "fws/mysql:8.4"); err == nil || !strings.Contains(err.Error(), "password option is not specified") {
		t.Errorf("a run without a password option: %v", err)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Testcontainers' Valkey module runs valkey-server itself and waits for
// the port from inside the container too.
func TestValkey(t *testing.T) {
	ctx := context.Background()
	c, err := tcvalkey.Run(ctx, "fws/valkey:9")
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(uri, "redis://"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	for _, step := range [][2]string{
		{"*3\r\n$3\r\nSET\r\n$4\r\nnote\r\n$4\r\nkept\r\n", "+OK"},
		{"*2\r\n$3\r\nGET\r\n$4\r\nnote\r\n", "$4"},
	} {
		conn.Write([]byte(step[0]))
		if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != step[1] {
			t.Fatalf("%q answered %q", step[0], line)
		}
	}
	if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != "kept" {
		t.Fatalf("GET read %q", line)
	}
	// The image's own entrypoint runs it too, healthy.
	waitedRun(t, "fws-e2e-valkey", "fws/valkey:9")
}

func TestMemcached(t *testing.T) {
	ctx := context.Background()
	c, err := tcmemcached.Run(ctx, "fws/memcached:1.6")
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := c.HostPort(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(conn, "set note 0 0 4\r\nkept\r\nget note\r\n")
	r := bufio.NewReader(conn)
	var got []string
	for len(got) < 4 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
		got = append(got, strings.TrimSpace(line))
	}
	if strings.Join(got, "|") != "STORED|VALUE note 0 4|kept|END" {
		t.Fatalf("memcached answered %q", got)
	}
	if out := mustDocker(t, "exec", c.GetContainerID(), "/entry", "health"); out != "" {
		t.Errorf("the health check printed %q", out)
	}
	if user := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Config.User}}", c.GetContainerID())); user != "memcache" {
		t.Errorf("memcached runs as %q", user)
	}
}

// Testcontainers' NATS module runs the server with JetStream; a client
// publishes to its own subscription. The image's own configuration serves
// the monitoring endpoint.
func TestNATS(t *testing.T) {
	ctx := context.Background()
	c, err := tcnats.Run(ctx, "fws/nats:2.14")
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(uri, "nats://"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	if info, _ := r.ReadString('\n'); !strings.HasPrefix(info, "INFO ") || !strings.Contains(info, `"jetstream":true`) {
		t.Fatalf("the server greeted with %q", info)
	}
	fmt.Fprint(conn, "CONNECT {\"verbose\":false}\r\nSUB notes 1\r\nPUB notes 4\r\nkept\r\nPING\r\n")
	var got []string
	for len(got) < 3 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
		got = append(got, strings.TrimSpace(line))
	}
	if strings.Join(got, "|") != "MSG notes 1 4|kept|PONG" {
		t.Fatalf("the server answered %q", got)
	}

	const name = "fws-e2e-nats"
	mustDocker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::8222", "fws/nats:2.14")
	defer docker(t, "rm", "-f", name)
	waitLog(t, name, "Server is ready")
	if body := httpGet(t, "http://"+strings.TrimSpace(mustDocker(t, "port", name, "8222"))+"/healthz"); !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("/healthz reads %q", body)
	}
}

// SeaweedFS runs every component in one process by default, mini, which
// advertises the address of the container's eth0 it finds among its
// interfaces; an S3 client stores an object through it.
func TestSeaweedFS(t *testing.T) {
	const name = "fws-e2e-seaweedfs"
	mustDocker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::8333", "fws/seaweedfs:4.36")
	defer docker(t, "rm", "-f", "-v", name)
	waitLog(t, name, "All enabled components are running and ready to use")
	ctx := context.Background()
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String("http://" + strings.TrimSpace(mustDocker(t, "port", name, "8333"))), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("any", "any", ""),
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("notes")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("notes"), Key: aws.String("note.txt"), Body: strings.NewReader("kept")}); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("notes"), Key: aws.String("note.txt")})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if b, _ := io.ReadAll(got.Body); string(b) != "kept" {
		t.Fatalf("the object reads %q", b)
	}
}

// Testcontainers' own modules run the catalog's older images: their log
// waits, and the port check they run inside the container with nc.
func TestTestcontainersModules(t *testing.T) {
	ctx := context.Background()
	t.Run("postgres", func(t *testing.T) {
		c, err := tcpostgres.Run(ctx, "fws/postgres:18", tcpostgres.WithDatabase("app"), tcpostgres.WithUsername("app"), tcpostgres.WithPassword("apppw"),
			tcpostgres.BasicWaitStrategies())
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		dsn, err := c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var fsync string
		if err := conn.QueryRow(ctx, "SHOW fsync").Scan(&fsync); err != nil || fsync != "off" {
			t.Fatalf("fsync is %q, %v: the module's -c fsync=off did not reach the server", fsync, err)
		}
	})
	t.Run("redis", func(t *testing.T) {
		c, err := tcredis.Run(ctx, "fws/redis:8")
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := c.ConnectionString(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(uri, "redis://"), 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, "PING\r\n")
		if line, _ := bufio.NewReader(conn).ReadString('\n'); strings.TrimSpace(line) != "+PONG" {
			t.Fatalf("PING answered %q", line)
		}
	})
	t.Run("rabbitmq", func(t *testing.T) {
		c, err := tcrabbitmq.Run(ctx, "fws/rabbitmq:4", tcrabbitmq.WithAdminUsername("tc-admin"), tcrabbitmq.WithAdminPassword("tc-pass"))
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		url, err := c.AmqpURL(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := amqp.Dial(url)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		conn.Close()
	})
	// The modules wait for the server's "ready for connections" line to
	// end in the vendor build's compilation comment.
	versionComment := func(t *testing.T, dsn, want string) {
		t.Helper()
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var got string
		if err := db.QueryRow("SELECT @@version_comment").Scan(&got); err != nil || got != want {
			t.Fatalf("version_comment is %q, %v, want %q", got, err, want)
		}
	}
	t.Run("mysql", func(t *testing.T) {
		c, err := tcmysql.Run(ctx, "fws/mysql:8.4")
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		dsn, err := c.ConnectionString(ctx)
		if err != nil {
			t.Fatal(err)
		}
		versionComment(t, dsn, "MySQL Community Server - GPL")
	})
	t.Run("mariadb", func(t *testing.T) {
		c, err := tcmariadb.Run(ctx, "fws/mariadb:11")
		testcontainers.CleanupContainer(t, c)
		if err != nil {
			t.Fatal(err)
		}
		dsn, err := c.ConnectionString(ctx)
		if err != nil {
			t.Fatal(err)
		}
		versionComment(t, dsn, "mariadb.org binary distribution")
	})
}
