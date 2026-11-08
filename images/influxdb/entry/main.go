// Command entry runs InfluxDB 2 inside a fwsd container as the official
// image's entrypoint.sh does: it finds the configuration file, prepares the
// data directories, sets up a fresh database or upgrades a 1.x one when
// DOCKER_INFLUXDB_INIT_MODE asks, through a temporary server on
// INFLUXD_INIT_PORT that the scripts in /docker-entrypoint-initdb.d also
// see, and runs influxd as the influxdb user. Any other command runs as
// given.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/identity"
	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"
)

const (
	configVolume  = "/etc/influxdb2"
	userScriptDir = "/docker-entrypoint-initdb.d"
	initConfig    = "/tmp/config.json"
)

var configNames = []string{"config.json", "config.toml", "config.yaml", "config.yml"}

var logLevels = map[string]int{"error": 0, "warn": 1, "info": 2, "debug": 3}

var logLevel = "error"

// settings maps each setting the entrypoint reads to its environment
// variable and default, as influx server-config would report them.
var settings = map[string][2]string{
	"bolt-path":         {"INFLUXD_BOLT_PATH", "/var/lib/influxdb2/influxd.bolt"},
	"engine-path":       {"INFLUXD_ENGINE_PATH", "/var/lib/influxdb2/engine"},
	"log-level":         {"INFLUXD_LOG_LEVEL", "info"},
	"tls-key":           {"INFLUXD_TLS_KEY", ""},
	"tls-cert":          {"INFLUXD_TLS_CERT", ""},
	"http-bind-address": {"INFLUXD_HTTP_BIND_ADDRESS", ":8086"},
}

var requiredInitVars = []string{
	"DOCKER_INFLUXDB_INIT_USERNAME", "DOCKER_INFLUXDB_INIT_PASSWORD",
	"DOCKER_INFLUXDB_INIT_ORG", "DOCKER_INFLUXDB_INIT_BUCKET",
}

var boltPath, enginePath string

// cleanup, while armed, deletes the half-made database when the
// initialization fails, so that the next start initializes again.
var cleanup bool

// Log writes a line in InfluxDB's structured format, at or below the
// level influxd is configured with.
func Log(level, msg string, attrs ...string) {
	if logLevels[level] > logLevels[logLevel] {
		return
	}
	fmt.Fprintln(os.Stderr, Line(time.Now(), level, msg, attrs...))
}

func Line(t time.Time, level, msg string, attrs ...string) string {
	kv := `"system": "docker"`
	for i := 0; i+1 < len(attrs); i += 2 {
		kv += fmt.Sprintf(`, "%s": "%s"`, attrs[i], attrs[i+1])
	}
	return fmt.Sprintf("%s\t%s\t%s\t{%s}", t.UTC().Format("2006-01-02T15:04:05.000000000Z"), level, msg, kv)
}

func fail(msg string, attrs ...string) {
	Log("error", msg, attrs...)
	exit(1)
}

func exit(code int) {
	if cleanup {
		Log("warn", "cleaning bolt and engine files to prevent conflicts on retry", "bolt_path", boltPath, "engine_path", enginePath)
		os.RemoveAll(sandboxenv.Path(boltPath))
		entries, _ := filepath.Glob(sandboxenv.Path(enginePath) + "/*")
		for _, e := range entries {
			os.RemoveAll(e)
		}
	}
	os.Exit(code)
}

func setConfigPath() {
	path := "/etc/defaults/influxdb2/config.yml"
	if p := os.Getenv("INFLUXD_CONFIG_PATH"); p != "" {
		path = p
	} else {
		for _, name := range configNames {
			if st, err := os.Stat(sandboxenv.Path(configVolume + "/" + name)); err == nil && st.Mode().IsRegular() {
				path = configVolume + "/" + name
				break
			}
		}
	}
	os.Setenv("INFLUXD_CONFIG_PATH", path)
}

// ConfigGet reads a setting as influxd will: from its command line
// arguments first, then its environment variable, then the configuration
// file, and then the default.
func ConfigGet(key string, args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+key+"="); ok {
			return v
		}
		if strings.HasPrefix(a, "--"+key) {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
	}
	s := settings[key]
	if v, ok := os.LookupEnv(s[0]); ok {
		return v
	}
	if conf, err := readConfig(os.Getenv("INFLUXD_CONFIG_PATH")); err == nil {
		if v, ok := conf[key]; ok && v != nil {
			return fmt.Sprint(v)
		}
	}
	return s[1]
}

// readConfig reads a configuration file in the format its extension names.
func readConfig(path string) (map[string]any, error) {
	b, err := os.ReadFile(sandboxenv.Path(path))
	if err != nil {
		return nil, err
	}
	conf := map[string]any{}
	switch format(path) {
	case "toml":
		err = toml.Unmarshal(b, &conf)
	case "json":
		err = json.Unmarshal(b, &conf)
	case "yaml":
		err = yaml.Unmarshal(b, &conf)
	default:
		err = fmt.Errorf("%s: unknown configuration format", path)
	}
	return conf, err
}

func format(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, ".toml"):
		return "toml"
	case strings.HasSuffix(p, ".json"):
		return "json"
	case strings.HasSuffix(p, ".yaml"), strings.HasSuffix(p, ".yml"):
		return "yaml"
	}
	return ""
}

// createDirectories makes the data directories and the configuration
// volume, and as root gives what they hold to the influxdb user.
func createDirectories() {
	boltDir := filepath.Dir(boltPath)
	for _, d := range []string{boltDir, enginePath} {
		if err := os.MkdirAll(sandboxenv.Path(d), 0o755); err != nil {
			fail("cannot create a data directory", "path", d, "error", err.Error())
		}
		os.Chmod(sandboxenv.Path(d), 0o700)
	}
	os.MkdirAll(sandboxenv.Path(configVolume), 0o755)
	os.Chmod(sandboxenv.Path(configVolume), 0o775)
	if os.Getuid() != 0 {
		return
	}
	id, err := identity.Resolve(sandboxenv.Root(), "influxdb")
	if err != nil {
		fail("cannot find the influxdb user", "error", err.Error())
	}
	for _, d := range []string{boltDir, enginePath, configVolume} {
		err := filepath.WalkDir(sandboxenv.Path(d), func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			var st syscall.Stat_t
			if syscall.Stat(p, &st) == nil && st.Uid == id.UID {
				return nil
			}
			return os.Chown(p, int(id.UID), -1)
		})
		if err != nil {
			fail("cannot give a data directory to influxdb", "path", d, "error", err.Error())
		}
	}
}

// readInitSecretFiles reads the credentials that *_FILE variables name,
// such as Docker secrets.
func readInitSecretFiles() {
	for _, v := range []string{"DOCKER_INFLUXDB_INIT_PASSWORD", "DOCKER_INFLUXDB_INIT_USERNAME", "DOCKER_INFLUXDB_INIT_ADMIN_TOKEN"} {
		file := os.Getenv(v + "_FILE")
		if file == "" {
			continue
		}
		b, err := os.ReadFile(sandboxenv.Path(file))
		switch {
		case os.IsNotExist(err):
			Log("warn", "file does not exist, skipping", v+"_FILE", file)
		case err != nil:
			Log("warn", "file is not readable, skipping", v+"_FILE", file)
		default:
			os.Setenv(v, strings.TrimRight(string(b), "\n"))
		}
	}
}

func run(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func upgrade() {
	args := []string{"upgrade", "--force",
		"--username", os.Getenv("DOCKER_INFLUXDB_INIT_USERNAME"),
		"--password", os.Getenv("DOCKER_INFLUXDB_INIT_PASSWORD"),
		"--org", os.Getenv("DOCKER_INFLUXDB_INIT_ORG"),
		"--bucket", os.Getenv("DOCKER_INFLUXDB_INIT_BUCKET"),
		"--v2-config-path", configVolume + "/config.toml",
		"--influx-configs-path", os.Getenv("INFLUX_CONFIGS_PATH"),
		"--continuous-query-export-path", configVolume + "/v1-cq-export.txt",
		"--log-path", configVolume + "/upgrade.log",
		"--log-level", logLevel,
		"--bolt-path", boltPath,
		"--engine-path", enginePath,
		"--overwrite-existing-v2",
	}
	if r := os.Getenv("DOCKER_INFLUXDB_INIT_RETENTION"); r != "" {
		args = append(args, "--retention", r)
	}
	if t := os.Getenv("DOCKER_INFLUXDB_INIT_ADMIN_TOKEN"); t != "" {
		args = append(args, "--token", t)
	}
	v1Config, v1Dir := os.Getenv("DOCKER_INFLUXDB_INIT_UPGRADE_V1_CONFIG"), os.Getenv("DOCKER_INFLUXDB_INIT_UPGRADE_V1_DIR")
	switch {
	case v1Config != "" && isFile(v1Config):
		args = append(args, "--config-file", v1Config)
	case v1Dir != "" && isDir(v1Dir):
		args = append(args, "--v1-dir", v1Dir)
	case isFile("/etc/influxdb/influxdb.conf"):
		args = append(args, "--config-file", "/etc/influxdb/influxdb.conf")
	case isDir("/var/lib/influxdb"):
		args = append(args, "--v1-dir", "/var/lib/influxdb")
	default:
		fail("failed to autodetect usable V1 config or data dir, aborting upgrade")
	}
	if err := run("influxd", args...); err != nil {
		exit(1)
	}
	setConfigPath()
}

func isFile(p string) bool {
	st, err := os.Stat(sandboxenv.Path(p))
	return err == nil && st.Mode().IsRegular()
}

func isDir(p string) bool {
	st, err := os.Stat(sandboxenv.Path(p))
	return err == nil && st.IsDir()
}

// userScriptsPresent reports whether the scripts directory holds an
// executable *.sh file at any depth, as find -executable sees it.
func userScriptsPresent() bool {
	found := false
	filepath.WalkDir(sandboxenv.Path(userScriptDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil || found || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".sh") {
			return nil
		}
		found = syscall.Access(p, 1) == nil
		return nil
	})
	return found
}

// InitConfig is the configuration the temporary server runs with: the
// user's, on the init port, without TLS.
func InitConfig(path, bind string) ([]byte, error) {
	conf, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	conf["http-bind-address"] = bind
	delete(conf, "tls-cert")
	delete(conf, "tls-key")
	b, err := json.MarshalIndent(conf, "", "  ")
	return append(b, '\n'), err
}

// FinalPort is what follows the first colon of a bind address.
func FinalPort(bind string) string {
	if _, port, ok := strings.Cut(bind, ":"); ok {
		return port
	}
	return bind
}

func initInfluxd(args []string) {
	mode := os.Getenv("DOCKER_INFLUXDB_INIT_MODE")
	if mode != "setup" && mode != "upgrade" {
		fail("found invalid DOCKER_INFLUXDB_INIT_MODE, valid values are 'setup' and 'upgrade'", "DOCKER_INFLUXDB_INIT_MODE", mode)
	}
	missing := false
	for _, v := range requiredInitVars {
		if os.Getenv(v) == "" {
			Log("error", "missing parameter, cannot init InfluxDB", "parameter", v)
			missing = true
		}
	}
	if missing {
		exit(1)
	}
	cleanup = true
	if mode == "upgrade" {
		upgrade()
		if !userScriptsPresent() {
			cleanup = false
			return
		}
	}

	finalBind := ConfigGet("http-bind-address", args)
	initPort := os.Getenv("INFLUXD_INIT_PORT")
	initBind := ":" + initPort
	if initBind == finalBind {
		Log("warn", "influxd setup binding to same addr as final config, server will be exposed before ready", "addr", initBind)
	}
	scheme := "http"
	if ConfigGet("tls-cert", args) != "" && ConfigGet("tls-key", args) != "" {
		scheme = "https"
	}
	conf, err := InitConfig(os.Getenv("INFLUXD_CONFIG_PATH"), initBind)
	if err != nil {
		fail("cannot read the configuration", "path", os.Getenv("INFLUXD_CONFIG_PATH"), "error", err.Error())
	}
	os.Stdout.Write(conf)
	if err := os.WriteFile(sandboxenv.Path(initConfig), conf, 0o644); err != nil {
		fail("cannot write the setup configuration", "path", initConfig, "error", err.Error())
	}

	Log("info", "booting influxd server in the background")
	server := exec.Command("influxd")
	server.Stdout, server.Stderr = os.Stdout, os.Stderr
	server.Env = append(os.Environ(), "INFLUXD_CONFIG_PATH="+initConfig, "INFLUXD_HTTP_BIND_ADDRESS="+initBind,
		"INFLUXD_TLS_CERT=", "INFLUXD_TLS_KEY=")
	if err := server.Start(); err != nil {
		fail("cannot start influxd", "error", err.Error())
	}
	exited := make(chan struct{})
	go func() {
		server.Wait()
		close(exited)
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-signals
		server.Process.Signal(s)
		<-exited
		exit(0)
	}()

	os.Setenv("INFLUX_HOST", "http://localhost:"+initPort)
	waitForInfluxd(exited)
	if mode == "setup" {
		setup := []string{"setup", "--force",
			"--username", os.Getenv("DOCKER_INFLUXDB_INIT_USERNAME"),
			"--password", os.Getenv("DOCKER_INFLUXDB_INIT_PASSWORD"),
			"--org", os.Getenv("DOCKER_INFLUXDB_INIT_ORG"),
			"--bucket", os.Getenv("DOCKER_INFLUXDB_INIT_BUCKET"),
			"--name", os.Getenv("DOCKER_INFLUXDB_INIT_CLI_CONFIG_NAME"),
		}
		if r := os.Getenv("DOCKER_INFLUXDB_INIT_RETENTION"); r != "" {
			setup = append(setup, "--retention", r)
		}
		if t := os.Getenv("DOCKER_INFLUXDB_INIT_ADMIN_TOKEN"); t != "" {
			setup = append(setup, "--token", t)
		}
		if err := run("influx", setup...); err != nil {
			exit(1)
		}
	}
	setInitResourceIDs()
	if isDir(userScriptDir) {
		Log("info", "Executing user-provided scripts", "script_dir", userScriptDir)
		if err := run("run-parts", "--regex", ".*sh$", "--report", "--exit-on-error", userScriptDir); err != nil {
			exit(1)
		}
	}

	Log("info", "initialization complete, shutting down background influxd")
	signal.Stop(signals)
	server.Process.Signal(syscall.SIGTERM)
	<-exited
	cleanup = false

	configs := os.Getenv("INFLUX_CONFIGS_PATH")
	b, err := os.ReadFile(sandboxenv.Path(configs))
	if err != nil {
		fail("cannot read the CLI configurations", "path", configs, "error", err.Error())
	}
	b = []byte(strings.ReplaceAll(string(b), "http://localhost:"+initPort, scheme+"://localhost:"+FinalPort(finalBind)))
	if err := os.WriteFile(sandboxenv.Path(configs), b, 0o600); err != nil {
		fail("cannot write the CLI configurations", "path", configs, "error", err.Error())
	}
}

// waitForInfluxd pings the temporary server once a second, as the official
// entrypoint does, until it answers, exits or runs out of attempts.
func waitForInfluxd(exited chan struct{}) {
	attempts := 0
	fmt.Sscan(os.Getenv("INFLUXD_INIT_PING_ATTEMPTS"), &attempts)
	count := 0
	for count < attempts {
		select {
		case <-exited:
			fail("influxd crashed during startup", "total_pings", fmt.Sprint(count))
		case <-time.After(time.Second):
		}
		Log("info", "pinging influxd...", "ping_attempt", fmt.Sprint(count))
		count++
		ping := exec.Command("influx", "ping")
		if ping.Run() == nil {
			Log("info", "got response from influxd, proceeding", "total_pings", fmt.Sprint(count))
			return
		}
	}
	fail("influxd took too long to start up", "total_pings", fmt.Sprint(count))
}

// setInitResourceIDs exports the IDs of the user, organization and bucket
// setup made, for the user scripts; the user is the one
// DOCKER_INFLUXDB_INIT_USER names, as in the official entrypoint.
func setInitResourceIDs() {
	for _, r := range [][3]string{
		{"user", "DOCKER_INFLUXDB_INIT_USER", "DOCKER_INFLUXDB_INIT_USER_ID"},
		{"org", "DOCKER_INFLUXDB_INIT_ORG", "DOCKER_INFLUXDB_INIT_ORG_ID"},
		{"bucket", "DOCKER_INFLUXDB_INIT_BUCKET", "DOCKER_INFLUXDB_INIT_BUCKET_ID"},
	} {
		c := exec.Command("influx", r[0], "list", "-n", os.Getenv(r[1]), "--hide-headers")
		c.Stderr = os.Stderr
		out, err := c.Output()
		if err != nil {
			exit(1)
		}
		os.Setenv(r[2], FirstFields(string(out)))
	}
}

// FirstFields is cut -f 1 of each line, without the last newline.
func FirstFields(out string) string {
	var ids []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		id, _, _ := strings.Cut(line, "\t")
		ids = append(ids, id)
	}
	return strings.Join(ids, "\n")
}

// RunsInfluxd reports whether the arguments run the server, and the
// arguments influxd gets.
func RunsInfluxd(args []string) (bool, []string) {
	switch {
	case len(args) == 0 || args[0] == "run" || strings.HasPrefix(args[0], "-"):
	case args[0] == "influxd" && (len(args) == 1 || args[1] == "run" || strings.HasPrefix(args[1], "-")):
		args = args[1:]
	default:
		return false, args
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	return true, args
}

func main() {
	setConfigPath()
	runs, args := RunsInfluxd(os.Args[1:])
	if !runs {
		execute(args)
	}
	help := false
	for _, a := range args {
		help = help || a == "--help" || a == "-h"
	}
	if !help {
		logLevel = "info"
		if l := ConfigGet("log-level", args); l != "" {
			if _, ok := logLevels[l]; ok {
				logLevel = l
			}
		}
		boltPath, enginePath = ConfigGet("bolt-path", args), ConfigGet("engine-path", args)
		os.Setenv("BOLT_PATH", boltPath)
		os.Setenv("ENGINE_PATH", enginePath)
		createDirectories()
	}
	switch {
	case isFile(boltPath):
		Log("info", "found existing boltdb file, skipping setup wrapper", "bolt_path", boltPath)
	case os.Getenv("DOCKER_INFLUXDB_INIT_MODE") == "":
		Log("warn", "boltdb not found at configured path, but DOCKER_INFLUXDB_INIT_MODE not specified, skipping setup wrapper", "bolt_path", boltPath)
	default:
		readInitSecretFiles()
		initInfluxd(args)
		createDirectories()
	}
	if err := sandboxenv.Become("influxdb"); err != nil {
		fail("cannot run as influxdb", "error", err.Error())
	}
	execute(append([]string{"influxd"}, args...))
}

func execute(args []string) {
	if len(args) == 0 {
		fail("no command to run")
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		fail("cannot run the command", "command", args[0], "error", err.Error())
	}
	err = syscall.Exec(bin, args, os.Environ())
	fail("cannot run the command", "command", args[0], "error", err.Error())
}
