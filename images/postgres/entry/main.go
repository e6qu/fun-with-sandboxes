// Command entry initialises and runs PostgreSQL inside a fwsd container,
// configured from the environment the way the official image is. The
// server listens on the container's port 5432, which the container library
// puts on its host port, and on its sockets in /var/run/postgresql, as the
// official image's, and in /tmp, where the image's own libpq looks.
package main

import (
	"bufio"
	"compress/gzip"
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
	// Dynamic shared memory as files in the data directory: POSIX shared
	// memory names are confined to the container's prefix, as a private IPC
	// namespace confines them, and PostgreSQL picks its own.
	"dynamic_shared_memory_type=mmap",
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	user := getenv("POSTGRES_USER", "postgres")
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(run(bin("pg_isready"), "-q", "-h", "127.0.0.1", "-U", user, "-d", "postgres"))
	}
	pgdata := sandboxenv.Path(getenv("PGDATA", "/var/lib/postgresql/data"))
	sockets := sandboxenv.Path("/var/run/postgresql")
	if err := os.MkdirAll(sockets, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		log.Fatal(err)
	}
	if err := sandboxenv.Become("postgres", pgdata, sockets); err != nil {
		log.Fatal(err)
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "postgres" {
		args = args[1:]
	}
	if err := initialise(pgdata, user, sockets, args); err != nil {
		log.Fatal(err)
	}
	postgres := bin("postgres")
	// nixpkgs builds libpq with /tmp as its default socket directory, so
	// the image's psql finds the server there with no host named.
	server := append([]string{postgres}, serverArgs(pgdata, "*", sockets+","+sandboxenv.Path("/tmp"))...)
	log.Fatal(syscall.Exec(postgres, append(server, args...), os.Environ()))
}

// bin returns the real path of a PostgreSQL program. PostgreSQL finds its
// share and lib directories relative to its argv[0], looked up on PATH when
// bare, so argv[0] must be this path and not the /bin symlink.
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

func initialise(pgdata, user, sockets string, extra []string) error {
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
	// The database and init scripts go through a server that listens on
	// its socket alone, as the official image's entrypoint runs it, with
	// the server's own arguments.
	temp := exec.Command(bin("postgres"), append(serverArgs(pgdata, "", sockets), extra...)...)
	temp.Stdout = os.Stdout
	logs, err := temp.StderrPipe()
	if err != nil {
		return err
	}
	if err := temp.Start(); err != nil {
		return err
	}
	ready := make(chan struct{})
	go relay(logs, ready)
	exited := make(chan error, 1)
	go func() { exited <- temp.Wait() }()
	select {
	case <-ready:
	case err := <-exited:
		return fmt.Errorf("the setup server exited before it was ready: %v", err)
	}
	stop := func() error {
		temp.Process.Signal(syscall.SIGINT)
		return <-exited
	}
	psql := func(db string, stdin io.Reader, script string) error {
		args := []string{"-v", "ON_ERROR_STOP=1", "--username", user, "--no-password", "--no-psqlrc", "--host", sockets, "--dbname", db}
		if script != "" {
			args = append(args, "-f", script)
		}
		cmd := exec.Command(bin("psql"), args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	db := getenv("POSTGRES_DB", user)
	if db != "postgres" {
		if err := psql("postgres", strings.NewReader("CREATE DATABASE "+ident(db)+";\n"), ""); err != nil {
			stop()
			return fmt.Errorf("create database %s: %w", db, err)
		}
	}
	scripts, _ := filepath.Glob(filepath.Join(sandboxenv.Path("/docker-entrypoint-initdb.d"), "*"))
	sort.Strings(scripts)
	for _, s := range scripts {
		if err := initScript(s, db, psql); err != nil {
			stop()
			return err
		}
	}
	if err := stop(); err != nil {
		return fmt.Errorf("the setup server: %w", err)
	}
	log.Printf("PostgreSQL init process complete; ready for start up.")
	return nil
}

// initScript runs an init script as the official image does: SQL, plain
// or gzipped, through psql into the database, and a shell script with the
// image's shell; it ignores any other file.
func initScript(path, db string, psql func(string, io.Reader, string) error) error {
	name := filepath.Base(path)
	switch {
	case strings.HasSuffix(name, ".sql"):
		log.Printf("running %s", name)
		return psql(db, nil, path)
	case strings.HasSuffix(name, ".sql.gz"):
		log.Printf("running %s", name)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return psql(db, z, "")
	case strings.HasSuffix(name, ".sh"):
		log.Printf("running %s", name)
		cmd := exec.Command(sandboxenv.Path("/bin/sh"), path)
		if fi, err := os.Stat(path); err == nil && fi.Mode()&0o111 != 0 {
			cmd = exec.Command(path)
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	log.Printf("ignoring %s", name)
	return nil
}

// relay copies the server's log to the entry's and closes ready when the
// server logs that it accepts connections.
func relay(r io.Reader, ready chan<- struct{}) {
	sc := bufio.NewScanner(r)
	announced := false
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(os.Stderr, line)
		if !announced && strings.Contains(line, "ready to accept connections") {
			announced = true
			close(ready)
		}
	}
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// serverArgs are the server's arguments: its data directory, the address
// it listens on ("" for its sockets alone), its socket directories,
// and the settings macOS's limits ask for.
func serverArgs(pgdata, listen, sockets string) []string {
	args := []string{"-D", pgdata, "-c", "listen_addresses=" + listen, "-c", "unix_socket_directories=" + sockets}
	for _, s := range serverSettings {
		args = append(args, "-c", s)
	}
	return args
}
