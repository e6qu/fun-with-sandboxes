// Command entry initialises and runs MongoDB inside a fwsd container as
// the official image's docker-entrypoint.sh does: a first argument that is
// an option runs mongod with it; mongod runs as the mongodb user; with
// MONGO_INITDB_ROOT_USERNAME and MONGO_INITDB_ROOT_PASSWORD, or scripts in
// /docker-entrypoint-initdb.d, an empty database is set up through a
// server that listens on loopback alone, which creates the root user and
// runs the scripts, and mongod then requires authentication; and mongod
// binds every address unless its options or configuration name some. The
// server listens on the container's port 27017, which the container
// library puts on its host port.
package main

import (
	"bufio"
	"encoding/json"
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

const initdbDir = "/docker-entrypoint-initdb.d"

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	args := os.Args[1:]
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = append([]string{"mongod"}, args...)
	}
	if len(args) == 0 {
		log.Fatal("no command to run")
	}
	if args[0] == "mongod" || args[0] == "mongos" {
		if err := sandboxenv.Become("mongodb", sandboxenv.Path("/data/configdb"), sandboxenv.Path("/data/db")); err != nil {
			log.Fatal(err)
		}
	}
	if args[0] == "mongod" {
		var err error
		if args, err = prepare(args); err != nil {
			log.Fatal(err)
		}
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// prepare sets the database up where the environment asks for it and
// returns mongod's arguments as the official entrypoint completes them.
func prepare(args []string) ([]string, error) {
	user, err := fileEnv("MONGO_INITDB_ROOT_USERNAME")
	if err != nil {
		return nil, err
	}
	pass, err := fileEnv("MONGO_INITDB_ROOT_PASSWORD")
	if err != nil {
		return nil, err
	}
	if (user == "") != (pass == "") {
		return nil, fmt.Errorf("error: missing 'MONGO_INITDB_ROOT_USERNAME' or 'MONGO_INITDB_ROOT_PASSWORD'\n" +
			"       both must be specified for a user to be created")
	}
	scripts, _ := filepath.Glob(filepath.Join(sandboxenv.Path(initdbDir), "*"))
	sort.Strings(scripts)
	initdb := user != "" || len(scripts) > 0
	if user != "" {
		args = ensureArg(args, "--auth")
	}
	for _, a := range []string{"--help", "-h", "--version", "--sysinfo"} {
		if haveArg(args, a) {
			initdb = false
		}
	}
	config := ""
	if haveArg(args, "--config") || haveArg(args, "-f") {
		if config, err = effectiveConfig(args); err != nil {
			return nil, err
		}
	}
	if initdb {
		dbPath := dbPathOf(args, config)
		for _, f := range []string{"WiredTiger", "journal", "local.0", "storage.bson"} {
			if _, err := os.Stat(filepath.Join(sandboxenv.Path(dbPath), f)); err == nil {
				initdb = false
			}
		}
	}
	if initdb {
		if err := initialise(args, user, pass, scripts); err != nil {
			return nil, err
		}
	}
	// MongoDB binds loopback alone by default; the image binds every
	// address unless told otherwise.
	if !haveArg(args, "--bind_ip") && !haveArg(args, "--bind_ip_all") && !configBinds(config) {
		args = append(args, "--bind_ip_all")
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "MONGO_INITDB_") {
			os.Unsetenv(k)
		}
	}
	return args, nil
}

// initialise runs a mongod on loopback alone, without authentication and
// standalone, creates the root user, runs the init scripts and stops it.
func initialise(args []string, user, pass string, scripts []string) error {
	hacked := append([]string{}, args...)
	hacked = removeArg(hacked, "--bind_ip_all", false)
	hacked = setArg(hacked, "--bind_ip", "127.0.0.1")
	hacked = setArg(hacked, "--port", "27017")
	hacked = removeArg(hacked, "--auth", false)
	// A key file implies authorization.
	hacked = removeArg(hacked, "--keyFile", true)
	if user != "" {
		hacked = removeArg(hacked, "--replSet", true)
	}
	tlsMode := "disabled"
	if haveArg(hacked, "--tlsCertificateKeyFile") {
		tlsMode = "allowTLS"
	}
	hacked = setArg(hacked, "--tlsMode", tlsMode)
	bin, err := exec.LookPath(hacked[0])
	if err != nil {
		return err
	}
	temp := exec.Command(bin, hacked[1:]...)
	temp.Stderr = os.Stderr
	logs, err := temp.StdoutPipe()
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
	stop := func() error {
		temp.Process.Signal(syscall.SIGTERM)
		return <-exited
	}
	select {
	case <-ready:
	case err := <-exited:
		return fmt.Errorf("error: %s does not appear to have stayed running -- perhaps it had an error? (%v)", args[0], err)
	}
	mongo := []string{"--host", "127.0.0.1", "--port", "27017", "--quiet"}
	if user != "" {
		js := fmt.Sprintf("db.createUser({user: %s, pwd: %s, roles: [{role: 'root', db: 'admin'}]})", jsString(user), jsString(pass))
		if err := mongosh(append(mongo, "admin", "--eval", js)...); err != nil {
			stop()
			return fmt.Errorf("create the root user: %w", err)
		}
	}
	database := os.Getenv("MONGO_INITDB_DATABASE")
	if database == "" {
		database = "test"
	}
	os.Setenv("MONGO_INITDB_DATABASE", database)
	fmt.Println()
	for _, f := range scripts {
		name := initdbDir + "/" + filepath.Base(f)
		switch {
		case strings.HasSuffix(f, ".sh"):
			fmt.Printf("%s: running %s\n", os.Args[0], name)
			// The official entrypoint sources the script in its own bash.
			sh := exec.Command("bash", "-c", `. "$1"`, "bash", f)
			sh.Stdout, sh.Stderr = os.Stdout, os.Stderr
			if err := sh.Run(); err != nil {
				stop()
				return fmt.Errorf("%s: %w", name, err)
			}
		case strings.HasSuffix(f, ".js"):
			fmt.Printf("%s: running %s\n", os.Args[0], name)
			if err := mongosh(append(mongo, database, f)...); err != nil {
				stop()
				return fmt.Errorf("%s: %w", name, err)
			}
			fmt.Println()
		default:
			fmt.Printf("%s: ignoring %s\n", os.Args[0], name)
		}
		fmt.Println()
	}
	// mongod --shutdown, which the official entrypoint runs, is Linux's
	// alone; mongod shuts down as cleanly on SIGTERM.
	if err := stop(); err != nil {
		return fmt.Errorf("the setup server: %w", err)
	}
	fmt.Println()
	fmt.Println("MongoDB init process complete; ready for start up.")
	fmt.Println()
	return nil
}

func mongosh(args ...string) error {
	c := exec.Command("mongosh", args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

// relay copies the setup server's log to the entry's and closes ready
// when the server logs that it waits for connections.
func relay(r io.Reader, ready chan<- struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	announced := false
	for sc.Scan() {
		line := sc.Text()
		fmt.Println(line)
		if !announced && strings.Contains(line, "Waiting for connections") {
			announced = true
			close(ready)
		}
	}
	io.Copy(os.Stdout, r)
}

// jsString quotes s as a JavaScript string.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// fileEnv reads name, or the file name_FILE names, not both.
func fileEnv(name string) (string, error) {
	v, file := os.Getenv(name), os.Getenv(name+"_FILE")
	if v != "" && file != "" {
		return "", fmt.Errorf("error: both %s and %s_FILE are set (but are exclusive)", name, name)
	}
	if file != "" {
		b, err := os.ReadFile(sandboxenv.Path(file))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return v, nil
}

// haveArg reports whether args hold option name, alone or with =value.
func haveArg(args []string, name string) bool {
	for _, a := range args[1:] {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func ensureArg(args []string, name string) []string {
	if haveArg(args, name) {
		return args
	}
	return append(args, name)
}

// removeArg leaves option name out of args, and with value set, the
// value that follows it.
func removeArg(args []string, name string, value bool) []string {
	out := args[:1:1]
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == name:
			if value {
				i++
			}
		case strings.HasPrefix(args[i], name+"="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

func setArg(args []string, name, value string) []string {
	return append(removeArg(args, name, true), name, value)
}

// argValue is the value of option name in args.
func argValue(args []string, name string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(args[i], name+"="); ok {
			return v
		}
	}
	return ""
}

// effectiveConfig is the configuration mongod reads from its options and
// configuration file, as mongod --outputConfig prints it.
func effectiveConfig(args []string) (string, error) {
	file := argValue(args, "--config")
	if file == "" {
		file = argValue(args, "-f")
	}
	out, err := exec.Command(args[0], "--config", file, "--outputConfig").Output()
	if err != nil {
		return "", fmt.Errorf("read the configuration file %s: %w", file, err)
	}
	return string(out), nil
}

// configBinds reports whether the configuration names addresses to bind.
func configBinds(config string) bool {
	for _, l := range strings.Split(config, "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(l), ":")
		if k == "bindIp" || k == "bindIpAll" {
			return true
		}
	}
	return false
}

// dbPathOf is the data directory mongod uses: the one its options or
// configuration name, or the image's for a config server or any other.
func dbPathOf(args []string, config string) string {
	if p := argValue(args, "--dbpath"); p != "" {
		return p
	}
	for _, l := range strings.Split(config, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "dbPath:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if haveArg(args, "--configsvr") || strings.Contains(config, "clusterRole: configsvr") {
		return "/data/configdb"
	}
	return "/data/db"
}
