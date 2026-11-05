// Command entry initialises and runs MySQL inside a fwsd container,
// configured from the environment the way the official image's entrypoint
// does: the server reads /etc/my.cnf and /etc/mysql/conf.d, and listens on
// the container's ports 3306 and 33060, which the container library puts
// on their host ports.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := handshake("127.0.0.1:3306"); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "mysqld" && !strings.HasPrefix(args[0], "-") {
		bin, err := exec.LookPath(args[0])
		if err != nil {
			log.Fatal(err)
		}
		log.Fatal(syscall.Exec(bin, args, os.Environ()))
	}
	if len(args) > 0 && args[0] == "mysqld" {
		args = args[1:]
	}
	server, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/mysqld"))
	if err != nil {
		log.Fatal(err)
	}
	base := filepath.Dir(filepath.Dir(server))
	datadir := sandboxenv.Path("/var/lib/mysql")
	files := sandboxenv.Path("/var/lib/mysql-files")
	run := sandboxenv.Path("/var/run/mysqld")
	tmp := sandboxenv.Path("/tmp")
	for _, d := range []string{datadir, files, run, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	if err := sandboxenv.Become("mysql", datadir, files, run); err != nil {
		log.Fatal(err)
	}
	// The server's compiled-in paths are Nix store paths a container cannot
	// read; every one it needs is named from the image. /etc/my.cnf names
	// the rest, as the official image's does.
	common := []string{
		"--basedir=" + base,
		"--lc-messages-dir=" + filepath.Join(base, "share", "mysql"),
		"--plugin-dir=" + filepath.Join(base, "lib", "mysql", "plugin"),
		"--datadir=" + datadir,
		"--secure-file-priv=" + files,
		"--tmpdir=" + tmp,
		"--socket=" + filepath.Join(run, "mysqld.sock"),
		"--pid-file=" + filepath.Join(run, "mysqld.pid"),
	}
	if err := initialise(server, datadir, common); err != nil {
		log.Fatal(err)
	}
	argv := append(append([]string{"mysqld"}, common...), "--mysqlx-socket="+filepath.Join(run, "mysqlx.sock"))
	log.Fatal(syscall.Exec(server, append(argv, args...), os.Environ()))
}

// initialise creates the data directory, and the users and database the
// environment asks for, where the data directory holds none.
func initialise(server, datadir string, common []string) error {
	if _, err := os.Stat(filepath.Join(datadir, "mysql")); err == nil {
		log.Printf("MySQL Database directory appears to contain a database; Skipping initialization")
		return nil
	}
	if os.Getenv("MYSQL_USER") == "root" {
		return errors.New(`MYSQL_USER="root", MYSQL_USER and MYSQL_PASSWORD are for configuring a regular user and cannot be used for the root user` + "\n" +
			"\tRemove MYSQL_USER=\"root\" and use one of the following to control the root user password:\n" +
			"\t- MYSQL_ROOT_PASSWORD\n\t- MYSQL_ALLOW_EMPTY_PASSWORD\n\t- MYSQL_RANDOM_ROOT_PASSWORD")
	}
	rootPassword := os.Getenv("MYSQL_ROOT_PASSWORD")
	switch {
	case rootPassword != "":
	case os.Getenv("MYSQL_ALLOW_EMPTY_PASSWORD") != "":
	case os.Getenv("MYSQL_RANDOM_ROOT_PASSWORD") != "":
		b := make([]byte, 16)
		rand.Read(b)
		rootPassword = hex.EncodeToString(b)
		log.Printf("GENERATED ROOT PASSWORD: %s", rootPassword)
	default:
		return errors.New("Database is uninitialized and password option is not specified\n" +
			"\tYou need to specify one of the following as an environment variable:\n" +
			"\t- MYSQL_ROOT_PASSWORD\n\t- MYSQL_ALLOW_EMPTY_PASSWORD\n\t- MYSQL_RANDOM_ROOT_PASSWORD")
	}
	log.Printf("Initializing database files")
	boot := exec.Command(server, append(common, "--initialize-insecure", "--mysqlx=OFF")...)
	boot.Args[0] = "mysqld"
	boot.Stdout, boot.Stderr = os.Stdout, os.Stderr
	fail := func(err error) error {
		entries, _ := os.ReadDir(datadir)
		for _, e := range entries {
			os.RemoveAll(filepath.Join(datadir, e.Name()))
		}
		return err
	}
	if err := boot.Run(); err != nil {
		return fail(fmt.Errorf("mysqld --initialize-insecure: %w", err))
	}
	log.Printf("Database files initialized")

	// The rest goes through a server that listens on its socket alone, as
	// the official image's entrypoint runs it, so no client reaches the
	// database before it is set up.
	socket := ""
	for _, a := range common {
		if v, ok := strings.CutPrefix(a, "--socket="); ok {
			socket = v
		}
	}
	temp := exec.Command(server, append(common, "--skip-networking", "--mysqlx=OFF")...)
	temp.Args[0] = "mysqld"
	temp.Stdout = os.Stdout
	logs, err := temp.StderrPipe()
	if err != nil {
		return fail(err)
	}
	if err := temp.Start(); err != nil {
		return fail(err)
	}
	ready := make(chan struct{})
	go relay(logs, ready)
	exited := make(chan error, 1)
	go func() { exited <- temp.Wait() }()
	select {
	case <-ready:
	case err := <-exited:
		return fail(fmt.Errorf("the setup server exited before it was ready: %v", err))
	}
	db, err := sql.Open("mysql", "root@unix("+socket+")/?multiStatements=true")
	if err != nil {
		temp.Process.Kill()
		<-exited
		return fail(err)
	}
	defer db.Close()
	if err := setup(db, rootPassword); err != nil {
		temp.Process.Kill()
		<-exited
		return fail(err)
	}
	if _, err := db.Exec("SHUTDOWN"); err != nil {
		temp.Process.Kill()
		<-exited
		return fail(fmt.Errorf("shut the setup server down: %w", err))
	}
	if err := <-exited; err != nil {
		return fail(fmt.Errorf("the setup server: %w", err))
	}
	log.Printf("MySQL init process done. Ready for start up.")
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
		if !announced && strings.Contains(line, "ready for connections") {
			announced = true
			close(ready)
		}
	}
}

// setup creates the users and database the environment asks for and runs
// the init scripts, as the official image's entrypoint does, as root,
// still without a password.
func setup(db *sql.DB, rootPassword string) error {
	host := os.Getenv("MYSQL_ROOT_HOST")
	if host == "" {
		host = "%"
	}
	var stmts []string
	add := func(format string, a ...any) { stmts = append(stmts, fmt.Sprintf(format, a...)) }
	add("SET @@SESSION.SQL_LOG_BIN=0")
	add("SET @@SESSION.SQL_MODE=REPLACE(@@SESSION.SQL_MODE, 'NO_BACKSLASH_ESCAPES', '')")
	add("ALTER USER 'root'@'localhost' IDENTIFIED BY %s", quote(rootPassword))
	if host != "localhost" {
		add("CREATE USER 'root'@%s IDENTIFIED BY %s", quote(host), quote(rootPassword))
		add("GRANT ALL ON *.* TO 'root'@%s WITH GRANT OPTION", quote(host))
	}
	if os.Getenv("MYSQL_ONETIME_PASSWORD") != "" {
		add("ALTER USER 'root'@%s PASSWORD EXPIRE", quote(host))
	}
	add("DROP DATABASE IF EXISTS test")
	dbName := os.Getenv("MYSQL_DATABASE")
	if dbName != "" {
		add("CREATE DATABASE IF NOT EXISTS %s", ident(dbName))
	}
	if user := os.Getenv("MYSQL_USER"); user != "" {
		add("CREATE USER %s@'%%' IDENTIFIED BY %s", quote(user), quote(os.Getenv("MYSQL_PASSWORD")))
		if dbName != "" {
			add("GRANT ALL ON %s.* TO %s@'%%'", ident(dbName), quote(user))
		}
	}
	// One connection holds the session settings for every statement.
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, s := range stmts {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(s, " IDENTIFIED", 2)[0], err)
		}
	}
	scripts, _ := filepath.Glob(filepath.Join(sandboxenv.Path("/docker-entrypoint-initdb.d"), "*"))
	sort.Strings(scripts)
	for _, s := range scripts {
		script, err := readScript(s)
		if err != nil {
			return err
		}
		if script == "" {
			log.Printf("ignoring %s", filepath.Base(s))
			continue
		}
		log.Printf("running %s", filepath.Base(s))
		if dbName != "" {
			script = "USE " + ident(dbName) + ";\n" + script
		}
		if _, err := conn.ExecContext(context.Background(), script); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(s), err)
		}
	}
	return nil
}

// readScript reads an init script the server runs: .sql, or .sql.gz
// uncompressed; any other file yields "".
func readScript(path string) (string, error) {
	switch {
	case strings.HasSuffix(path, ".sql"):
		b, err := os.ReadFile(path)
		return string(b), err
	case strings.HasSuffix(path, ".sql.gz"):
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		b, err := io.ReadAll(z)
		return string(b), err
	}
	return "", nil
}

func ident(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// quote makes s an SQL string literal.
func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// handshake reads the server's greeting, which it sends once it accepts
// connections.
func handshake(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, 5)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	// The payload of an error packet starts with 0xff; a greeting's with
	// the protocol version, 10.
	if head[4] != 10 {
		return fmt.Errorf("the server answered with packet type %#x, not a greeting", head[4])
	}
	return nil
}
