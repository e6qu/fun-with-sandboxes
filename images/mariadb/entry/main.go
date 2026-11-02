// Command entry initialises and runs MariaDB inside a fwsd container,
// configured from the environment the way the official image is, with the
// listen port and paths the engine assigned.
//
// It initialises a data directory as mariadb-install-db does, by feeding
// the server's own system table scripts to mariadbd --bootstrap: the
// script's interpreter is a Nix store path, which a container cannot run.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	port, err := sandboxenv.Port("3306")
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := handshake("127.0.0.1:" + port); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	server, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/mariadbd"))
	if err != nil {
		log.Fatal(err)
	}
	base := filepath.Dir(filepath.Dir(server))
	datadir := sandboxenv.Path("/var/lib/mysql")
	run := sandboxenv.Path("/run/mysqld")
	tmp := sandboxenv.Path("/tmp")
	for _, d := range []string{datadir, run, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	if err := sandboxenv.Become("mysql", datadir, run); err != nil {
		log.Fatal(err)
	}
	// The server's own paths are Nix store paths a container cannot read;
	// every one it needs is named from the image.
	common := []string{
		"--no-defaults",
		"--basedir=" + base,
		"--datadir=" + datadir,
		"--lc-messages-dir=" + filepath.Join(base, "share", "mysql"),
		"--plugin-dir=" + filepath.Join(base, "lib", "mysql", "plugin"),
		"--tmpdir=" + tmp,
		"--socket=" + filepath.Join(run, "mysqld.sock"),
		"--pid-file=" + filepath.Join(run, "mysqld.pid"),
	}
	if err := initialise(server, base, datadir, common); err != nil {
		log.Fatal(err)
	}
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "mariadbd" || args[0] == "mysqld") {
		args = args[1:]
	}
	argv := append([]string{"mariadbd"}, common...)
	argv = append(argv, "--port="+port, "--bind-address=127.0.0.1", "--skip-name-resolve", "--log-error=")
	log.Fatal(syscall.Exec(server, append(argv, args...), os.Environ()))
}

// listing describes the data directory a failed initialisation left, for
// its error.
func listing(dir string) string {
	var b strings.Builder
	// A volume's mount point is a symlink to the volume.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&b, "%s: %v\n", p, err)
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			fmt.Fprintf(&b, "%s: %v\n", p, err)
			return nil
		}
		target, _ := os.Readlink(p)
		fmt.Fprintf(&b, "%s %v %d %s\n", p, fi.Mode(), fi.Size(), target)
		return nil
	})
	return b.String()
}

func getenv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// initialise creates the system tables and the users and database the
// environment asks for, in a data directory that has none.
func initialise(server, base, datadir string, common []string) error {
	if _, err := os.Stat(filepath.Join(datadir, "mysql")); err == nil {
		log.Printf("MariaDB data directory appears to contain a database; skipping initialization")
		return nil
	}
	rootPassword := getenv("MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD")
	switch {
	case rootPassword != "":
	case getenv("MARIADB_ALLOW_EMPTY_ROOT_PASSWORD", "MYSQL_ALLOW_EMPTY_PASSWORD") != "":
	case getenv("MARIADB_RANDOM_ROOT_PASSWORD", "MYSQL_RANDOM_ROOT_PASSWORD") != "":
		b := make([]byte, 16)
		rand.Read(b)
		rootPassword = hex.EncodeToString(b)
		log.Printf("GENERATED ROOT PASSWORD: %s", rootPassword)
	default:
		return errors.New("Database is uninitialized and password option is not specified\n" +
			"\tYou need to specify one of MARIADB_ROOT_PASSWORD, MARIADB_ALLOW_EMPTY_ROOT_PASSWORD and MARIADB_RANDOM_ROOT_PASSWORD")
	}
	var system bytes.Buffer
	// As mariadb-install-db does, the system database comes first.
	system.WriteString("CREATE DATABASE IF NOT EXISTS mysql;\nuse mysql;\nSET @auth_root_socket=NULL;\n")
	share := filepath.Join(base, "share", "mysql")
	// The scripts mariadb-install-db feeds the server, in its order.
	for _, f := range []string{"mariadb_system_tables.sql", "mariadb_performance_tables.sql", "mariadb_system_tables_data.sql", "fill_help_tables.sql", "maria_add_gis_sp_bootstrap.sql", "mariadb_sys_schema.sql"} {
		b, err := os.ReadFile(filepath.Join(share, f))
		if err != nil {
			return err
		}
		system.Write(b)
		system.WriteString("\n")
	}
	boot := exec.Command(server, append(common, "--bootstrap", "--skip-networking", "--log-warnings=0", "--enforce-storage-engine=", "--max_allowed_packet=8M", "--net_buffer_length=16K")...)
	boot.Args[0] = "mariadbd"
	boot.Stdin, boot.Stdout, boot.Stderr = &system, os.Stdout, os.Stderr
	fail := func(err error) error {
		os.RemoveAll(filepath.Join(datadir, "mysql"))
		return err
	}
	if err := boot.Run(); err != nil {
		return fail(fmt.Errorf("mariadbd --bootstrap: %w\n%s", err, listing(datadir)))
	}

	// The rest goes through a server that listens on its socket alone, as
	// the official image's entrypoint runs it, so no client reaches the
	// database before it is set up.
	socket := ""
	for _, a := range common {
		if v, ok := strings.CutPrefix(a, "--socket="); ok {
			socket = v
		}
	}
	temp := exec.Command(server, append(common, "--skip-networking", "--log-error=")...)
	temp.Args[0] = "mariadbd"
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
	db, err := sql.Open("mysql", "root@unix("+socket+")/?multiStatements=true")
	if err != nil {
		temp.Process.Kill()
		return fail(err)
	}
	defer db.Close()
	select {
	case <-ready:
	case err := <-exited:
		return fail(fmt.Errorf("the setup server exited before it was ready: %v", err))
	}
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
	log.Printf("MariaDB init process done. Ready for start up.")
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
// the init scripts, as root, still without a password.
func setup(db *sql.DB, rootPassword string) error {
	host := getenv("MARIADB_ROOT_HOST", "MYSQL_ROOT_HOST")
	if host == "" {
		host = "%"
	}
	var stmts []string
	add := func(format string, a ...any) { stmts = append(stmts, fmt.Sprintf(format, a...)) }
	add("DELETE FROM mysql.global_priv WHERE User='' OR (User='root' AND Host NOT IN ('localhost'))")
	add("FLUSH PRIVILEGES")
	if host != "localhost" {
		add("CREATE USER 'root'@%s IDENTIFIED BY %s", quote(host), quote(rootPassword))
		add("GRANT ALL ON *.* TO 'root'@%s WITH GRANT OPTION", quote(host))
	}
	dbName := getenv("MARIADB_DATABASE", "MYSQL_DATABASE")
	if dbName != "" {
		add("CREATE DATABASE IF NOT EXISTS %s", ident(dbName))
	}
	if user := getenv("MARIADB_USER", "MYSQL_USER"); user != "" {
		add("CREATE USER %s@'%%' IDENTIFIED BY %s", quote(user), quote(getenv("MARIADB_PASSWORD", "MYSQL_PASSWORD")))
		if dbName != "" {
			add("GRANT ALL ON %s.* TO %s@'%%'", ident(dbName), quote(user))
		}
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(s, " IDENTIFIED", 2)[0], err)
		}
	}
	scripts, _ := filepath.Glob(filepath.Join(sandboxenv.Path("/docker-entrypoint-initdb.d"), "*.sql"))
	sort.Strings(scripts)
	for _, s := range scripts {
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		log.Printf("running %s", filepath.Base(s))
		script := string(b)
		if dbName != "" {
			script = "USE " + ident(dbName) + ";\n" + script
		}
		if _, err := db.Exec(script); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(s), err)
		}
	}
	// The root password comes last: the session authenticated without it.
	if _, err := db.Exec("ALTER USER 'root'@'localhost' IDENTIFIED BY " + quote(rootPassword)); err != nil {
		return fmt.Errorf("set the root password: %w", err)
	}
	return nil
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
	r := bufio.NewReader(c)
	head := make([]byte, 5)
	if _, err := io.ReadFull(r, head); err != nil {
		return err
	}
	// The payload of an error packet starts with 0xff; a greeting's with
	// the protocol version, 10.
	if head[4] != 10 {
		return fmt.Errorf("the server answered with packet type %#x, not a greeting", head[4])
	}
	return nil
}
