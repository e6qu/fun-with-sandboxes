// Command entry initialises and runs PostgreSQL inside a fwsd container,
// configured from the environment the way the official image is, with the
// listen port and paths the engine assigned.
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

// macOS ships small System V semaphore limits (87 semaphores machine-wide by
// default), so each server stays well inside them.
var serverSettings = []string{
	"max_connections=10",
	"max_worker_processes=2",
	"max_parallel_workers=0",
	"autovacuum_max_workers=1",
	"max_wal_senders=0",
	"wal_level=minimal",
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	port, err := sandboxenv.Port("5432")
	if err != nil {
		log.Fatal(err)
	}
	user := getenv("POSTGRES_USER", "postgres")
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(run(bin("pg_isready"), "-q", "-h", "127.0.0.1", "-p", port, "-U", user, "-d", "postgres"))
	}
	pgdata := sandboxenv.Path(getenv("PGDATA", "/var/lib/postgresql/data"))
	sockets := sandboxenv.Path("/var/run/postgresql")
	if err := os.MkdirAll(sockets, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := initialise(pgdata, user); err != nil {
		log.Fatal(err)
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "postgres" {
		args = args[1:]
	}
	server := []string{"postgres", "-D", pgdata,
		"-c", "listen_addresses=127.0.0.1", "-c", "port=" + port, "-c", "unix_socket_directories=" + sockets}
	for _, s := range serverSettings {
		server = append(server, "-c", s)
	}
	postgres := bin("postgres")
	server[0] = postgres
	log.Fatal(syscall.Exec(postgres, append(server, args...), os.Environ()))
}

// bin returns the real path of a PostgreSQL program. PostgreSQL finds its
// share and lib directories relative to the path it was started from, so it
// must not start from the /bin symlink.
func bin(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		log.Fatal(err)
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		log.Fatal(err)
	}
	return p
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(name string, args ...string) int {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	return 0
}

func initialise(pgdata, user string) error {
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err == nil {
		log.Printf("PostgreSQL database directory appears to contain a database; skipping initialization")
		return nil
	}
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		return err
	}
	args := []string{"-D", pgdata, "-U", user, "-E", "UTF8", "--no-locale"}
	password := os.Getenv("POSTGRES_PASSWORD")
	method := os.Getenv("POSTGRES_HOST_AUTH_METHOD")
	switch {
	case password != "":
		pw := filepath.Join(sandboxenv.Path("/tmp"), "pwfile")
		if err := os.MkdirAll(filepath.Dir(pw), 0o1777); err != nil {
			return err
		}
		if err := os.WriteFile(pw, []byte(password+"\n"), 0o600); err != nil {
			return err
		}
		defer os.Remove(pw)
		if method == "" {
			method = "scram-sha-256"
		}
		args = append(args, "--pwfile="+pw, "--auth-local=trust", "--auth-host="+method)
	case method == "trust":
		args = append(args, "--auth=trust")
	default:
		return errors.New("Database is uninitialized and superuser password is not specified.\n" +
			"       You must specify POSTGRES_PASSWORD to a non-empty value for the\n" +
			"       superuser, or set POSTGRES_HOST_AUTH_METHOD=trust to allow all connections without a password.")
	}
	if extra := os.Getenv("POSTGRES_INITDB_ARGS"); extra != "" {
		args = append(args, strings.Fields(extra)...)
	}
	if code := run(bin("initdb"), args...); code != 0 {
		return fmt.Errorf("initdb exited with %d", code)
	}
	db := getenv("POSTGRES_DB", user)
	if db != "postgres" {
		if err := single(pgdata, "postgres", strings.NewReader(fmt.Sprintf("CREATE DATABASE %q;\n", db))); err != nil {
			return err
		}
	}
	scripts, _ := filepath.Glob(filepath.Join(sandboxenv.Path("/docker-entrypoint-initdb.d"), "*.sql"))
	sort.Strings(scripts)
	for _, s := range scripts {
		f, err := os.Open(s)
		if err != nil {
			return err
		}
		log.Printf("running %s", filepath.Base(s))
		err = single(pgdata, db, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	log.Printf("PostgreSQL init process complete; ready for start up.")
	return nil
}

// single runs SQL through PostgreSQL's single-user mode, which needs no
// listening server.
func single(pgdata, db string, sql io.Reader) error {
	args := []string{"--single", "-j", "-D", pgdata}
	for _, s := range serverSettings {
		args = append(args, "-c", s)
	}
	cmd := exec.Command(bin("postgres"), append(args, db)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = sql, os.Stdout, os.Stderr
	return cmd.Run()
}
