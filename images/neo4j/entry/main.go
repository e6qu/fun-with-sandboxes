// Command entry runs Neo4j inside a fwsd container as the official image's
// docker-entrypoint.sh does: NEO4J_* variables, and files that *_FILE
// variables name, become settings in neo4j.conf, mounted /conf, /ssl,
// /plugins, /import, /logs and /licenses directories take their places,
// NEO4J_PLUGINS installs plugins, NEO4J_AUTH sets the initial password or
// turns authentication off, and the neo4j command runs the server that
// "neo4j console --dry-run" names, as the neo4j user.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

const pluginsFile = "/startup/neo4j-plugins.json"

// notConfigs are the NEO4J_ variables the image reads itself.
var notConfigs = map[string]bool{
	"NEO4J_ACCEPT_LICENSE_AGREEMENT": true, "NEO4J_AUTH": true, "NEO4J_AUTH_PATH": true, "NEO4J_DEBUG": true,
	"NEO4J_EDITION": true, "NEO4J_HOME": true, "NEO4J_PLUGINS": true, "NEO4J_SHA256": true, "NEO4J_TARBALL": true,
	"NEO4J_DEPRECATION_WARNING": true,
}

func main() {
	log.SetFlags(0)
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	home := os.Getenv("NEO4J_HOME")
	if home == "" {
		log.Fatal("NEO4J_HOME is not set")
	}
	conf := filepath.Join(home, "conf")
	root := os.Getuid() == 0
	if root {
		if err := os.Chmod(sandboxenv.Path(home), 0o700); err != nil {
			log.Fatal(err)
		}
		files, _ := filepath.Glob(sandboxenv.Path(conf) + "/*")
		for _, f := range files {
			if st, err := os.Lstat(f); err == nil && st.Mode().IsRegular() {
				if err := os.Chmod(f, 0o600); err != nil {
					log.Fatal(err)
				}
			}
		}
	}
	for _, kv := range os.Environ() {
		name, path, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "NEO4J_") || !strings.HasSuffix(name, "_FILE") {
			continue
		}
		b, err := os.ReadFile(sandboxenv.Path(path))
		if err != nil {
			log.Fatalf("The secret file '%s' does not exist or is not readable. Make sure you have correctly configured docker secrets.", path)
		}
		os.Setenv(strings.TrimSuffix(name, "_FILE"), strings.TrimRight(string(b), "\n"))
	}
	if strings.Contains(cmd, "neo4j") && os.Getenv("NEO4J_EDITION") == "enterprise" {
		if a := os.Getenv("NEO4J_ACCEPT_LICENSE_AGREEMENT"); a != "yes" && a != "eval" {
			log.Fatal("In order to use Neo4j Enterprise Edition you must accept the license agreement: set NEO4J_ACCEPT_LICENSE_AGREEMENT to yes or eval.")
		}
	}
	// The official script assigns these defaults to shell variables, so
	// they become settings without entering the server's environment.
	vars := map[string]string{}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "NEO4J_") {
			vars[name] = value
		}
	}
	setDefault := func(name, value string) {
		if vars[name] == "" {
			vars[name] = value
		}
	}
	if p := os.Getenv("NEO4JLABS_PLUGINS"); p != "" {
		log.Print("NEO4JLABS_PLUGINS has been renamed to NEO4J_PLUGINS since Neo4j 5.0.0.\nThe old name will still work, but is likely to be deprecated in future releases.")
		setDefault("NEO4J_PLUGINS", p)
	}
	if isDir("/conf") {
		if err := replaceConf(conf); err != nil {
			log.Fatal(err)
		}
	}
	if isDir("/ssl") {
		os.RemoveAll(sandboxenv.Path(home + "/certificates"))
		if err := os.Symlink("/ssl", sandboxenv.Path(home+"/certificates")); err != nil {
			log.Fatal(err)
		}
	}
	for _, d := range []struct{ dir, name string }{
		{"/plugins", "NEO4J_server_directories_plugins"},
		{"/import", "NEO4J_server_directories_import"},
		{"/logs", "NEO4J_server_directories_logs"},
		{"/licenses", "NEO4J_server_directories_licenses"},
	} {
		if isDir(d.dir) {
			setDefault(d.name, d.dir)
		}
	}
	if p := vars["NEO4J_PLUGINS"]; p != "" {
		if err := installPlugins(home, p); err != nil {
			log.Fatal(err)
		}
	}
	setDefault("NEO4J_db_tx__log_rotation_retention__policy", vars["NEO4J_dbms_tx__log_rotation_retention__policy"])
	setDefault("NEO4J_server_memory_pagecache_size", vars["NEO4J_dbms_memory_pagecache_size"])
	setDefault("NEO4J_server_default__listen__address", vars["NEO4J_dbms_default__listen__address"])
	neo4jConf := filepath.Join(conf, "neo4j.conf")
	for _, d := range [][2]string{{"server.memory.pagecache.size", "512M"}, {"server.default_listen_address", "0.0.0.0"}} {
		if err := addDefault(neo4jConf, d[0], d[1]); err != nil {
			log.Fatal(err)
		}
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		if notConfigs[name] || strings.HasSuffix(name, "_FILE") {
			continue
		}
		value := strings.TrimRight(vars[name], "\n")
		if value == "" {
			continue
		}
		setting := SettingName(name)
		if setting[0] >= '0' && setting[0] <= '9' {
			log.Printf("WARNING: %s not written to conf file. Settings that start with a number are not permitted.", setting)
			continue
		}
		file := neo4jConf
		if strings.HasPrefix(setting, "apoc.") {
			file = filepath.Join(conf, "apoc.conf")
		}
		if err := setSetting(file, setting, value); err != nil {
			log.Fatal(err)
		}
	}
	auth := vars["NEO4J_AUTH"]
	if p := vars["NEO4J_AUTH_PATH"]; p != "" {
		b, err := os.ReadFile(sandboxenv.Path(p))
		if err != nil {
			log.Fatalf("The password file '%s' does not exist", p)
		}
		auth = strings.TrimRight(string(b), "\n")
	}
	if cmd == "neo4j" {
		if err := setInitialPassword(neo4jConf, auth); err != nil {
			log.Fatal(err)
		}
	}
	os.Remove(sandboxenv.Path(home + "/run/neo4j.pid"))
	if s := os.Getenv("EXTENSION_SCRIPT"); s != "" && isFile(s) {
		if err := sourceExtension(s); err != nil {
			log.Fatal(err)
		}
	}
	if cmd == "dump-config" {
		if !isDir("/conf") {
			log.Fatal("You must mount a folder to /conf so that the configuration file(s) can be dumped to there.")
		}
		if err := copyDir(sandboxenv.Path(conf), sandboxenv.Path("/conf")); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Config Dumped")
		return
	}
	if err := sandboxenv.Become("neo4j", sandboxenv.Path(home), sandboxenv.Path("/data"), sandboxenv.Path("/logs")); err != nil {
		log.Fatal(err)
	}
	if cmd == "neo4j" {
		dry := []string{"console", "--dry-run"}
		if _, ok := os.LookupEnv("EXTENDED_CONF"); ok {
			dry = append(dry, "--expand-commands")
		}
		out, err := exec.Command("neo4j", dry...).Output()
		if err != nil {
			log.Fatalf("neo4j console --dry-run: %v", err)
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			log.Fatal("No Neo4j command was generated")
		}
		log.Fatal(syscall.Exec("/bin/sh", []string{"sh", "-c", "exec " + line}, os.Environ()))
	}
	if len(args) == 0 {
		log.Fatal("no command to run")
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// SettingName is the setting a NEO4J_ variable names: the rest of its name
// with "_" a dot and "__" an underscore.
func SettingName(name string) string {
	s := strings.ReplaceAll(strings.TrimPrefix(name, "NEO4J_"), "_", ".")
	return strings.ReplaceAll(s, "..", "_")
}

func isDir(p string) bool {
	st, err := os.Stat(sandboxenv.Path(p))
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(sandboxenv.Path(p))
	return err == nil && st.Mode().IsRegular()
}

// replaceConf empties conf and copies each file under /conf into it.
func replaceConf(conf string) error {
	dir := sandboxenv.Path(conf)
	old, _ := filepath.Glob(dir + "/*")
	for _, f := range old {
		if err := os.RemoveAll(f); err != nil {
			return err
		}
	}
	return filepath.WalkDir(sandboxenv.Path("/conf"), func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		return copyFile(p, filepath.Join(dir, d.Name()))
	})
}

func copyFile(from, to string) error {
	st, err := os.Stat(from)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.WriteFile(to, b, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chmod(to, st.Mode().Perm())
}

func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		return copyFile(p, dst)
	})
}

func readConf(file string) (string, error) {
	b, err := os.ReadFile(sandboxenv.Path(file))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

func appendConf(file, text string) error {
	f, err := os.OpenFile(sandboxenv.Path(file), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// hasLine reports whether a line of text starts with prefix.
func hasLine(text, prefix string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// addDefault appends setting=value to file, after a blank line, unless a
// line sets it.
func addDefault(file, setting, value string) error {
	text, err := readConf(file)
	if err != nil {
		return err
	}
	if hasLine(text, setting+"=") {
		return nil
	}
	return appendConf(file, "\n"+setting+"="+value+"\n")
}

// setSetting replaces the lines of file that set setting with one that
// sets it to value at its end; server.jvm.additional takes another line
// instead.
func setSetting(file, setting, value string) error {
	text, err := readConf(file)
	if err != nil {
		return err
	}
	if strings.Contains(text, setting+"=") && setting != "server.jvm.additional" {
		var kept []string
		for _, l := range strings.SplitAfter(text, "\n") {
			if !strings.HasPrefix(l, setting+"=") {
				kept = append(kept, l)
			}
		}
		if err := os.WriteFile(sandboxenv.Path(file), []byte(strings.Join(kept, "")), 0o600); err != nil {
			return err
		}
	}
	return appendConf(file, setting+"="+value+"\n")
}

var authPattern = regexp.MustCompile(`^([^/]+)/([^/]+)/?([tT][rR][uU][eE])?$`)

// setInitialPassword turns authentication off for NEO4J_AUTH=none, or sets
// the neo4j user's initial password for neo4j/PASSWORD, which a /true
// suffix makes the user change.
func setInitialPassword(neo4jConf, auth string) error {
	if auth == "none" {
		return setSetting(neo4jConf, "dbms.security.auth_enabled", "false")
	}
	m := authPattern.FindStringSubmatch(auth)
	if m == nil {
		if auth != "" {
			fmt.Println(auth + " is invalid")
			return fmt.Errorf("Invalid value for NEO4J_AUTH: '%s'", auth)
		}
		return nil
	}
	user, password, reset := m[1], m[2], m[3]
	if password == "neo4j" {
		return fmt.Errorf("Invalid value for password. It cannot be 'neo4j', which is the default.")
	}
	if user != "neo4j" {
		return fmt.Errorf("Invalid admin username, it must be neo4j.")
	}
	text, err := readConf(neo4jConf)
	if err != nil {
		return err
	}
	min := 8
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "dbms.security.auth_minimum_password_length") {
			if _, v, ok := strings.Cut(l, "="); ok {
				fmt.Sscan(v, &min)
			}
		}
	}
	if len(password) < min {
		log.Print("Invalid value for password. The minimum password length is 8 characters.\n" +
			"If Neo4j fails to start, you can:\n" +
			"  1) Use a stronger password.\n" +
			"  2) Set configuration dbms.security.auth_minimum_password_length to override the minimum password length requirement.\n" +
			"  3) Set environment variable NEO4J_dbms_security_auth__minimum__password__length to override the minimum password length requirement.")
	}
	args := []string{"dbms", "set-initial-password", password}
	if reset == "true" {
		args = append(args, "--require-password-change")
	}
	if _, ok := os.LookupEnv("EXTENDED_CONF"); ok {
		args = append(args, "--expand-commands")
	}
	if _, ok := os.LookupEnv("NEO4J_DEBUG"); ok {
		args = append(args, "--verbose")
	}
	c := exec.Command("neo4j-admin", args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

// sourceExtension runs EXTENSION_SCRIPT in bash, as the official script
// sources it, and takes the environment it leaves.
func sourceExtension(script string) error {
	out, err := exec.Command("bash", "-c", `. "$EXTENSION_SCRIPT" >&2 && env -0`).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", script, err)
	}
	os.Clearenv()
	for _, kv := range strings.Split(string(out), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			os.Setenv(k, v)
		}
	}
	return nil
}

// plugin is an entry of the image's neo4j-plugins.json.
type plugin struct {
	Location   string            `json:"location"`
	Versions   string            `json:"versions"`
	Properties map[string]string `json:"properties"`
}

// installPlugins installs the plugins the JSON list requested names, from
// the files in the image the plugins file locates, or else from the jar
// its versions file lists for this Neo4j, and applies their settings.
func installPlugins(home, requested string) error {
	b, err := os.ReadFile(sandboxenv.Path(pluginsFile))
	if err != nil {
		return err
	}
	var known map[string]plugin
	if err := json.Unmarshal(b, &known); err != nil {
		return err
	}
	var names []string
	if err := json.Unmarshal([]byte(requested), &names); err != nil {
		return fmt.Errorf("NEO4J_PLUGINS: %w", err)
	}
	for _, n := range names {
		if _, ok := known[n]; !ok {
			var options []string
			for k := range known {
				if !strings.HasPrefix(k, "_") {
					options = append(options, k)
				}
			}
			sort.Strings(options)
			return fmt.Errorf("%q is not a known Neo4j plugin. Options are:\n%s", n, strings.Join(options, "\n"))
		}
	}
	neo4jConf := filepath.Join(home, "conf", "neo4j.conf")
	reference, err := readConf(neo4jConf)
	if err != nil {
		return err
	}
	if err := appendConf(neo4jConf, ""); err != nil {
		return err
	}
	dir := filepath.Join(home, "plugins")
	if isDir("/plugins") {
		dir = "/plugins"
	}
	for _, n := range names {
		p := known[n]
		dest := filepath.Join(dir, n+".jar")
		var matches []string
		if p.Location != "" {
			matches, _ = filepath.Glob(sandboxenv.Path(p.Location))
		}
		if len(matches) > 0 {
			for _, m := range matches {
				fmt.Printf("Installing Plugin '%s' from %s to %s\n", n, p.Location, dest)
				if err := copyFile(m, sandboxenv.Path(dest)); err != nil {
					return err
				}
				st, err := os.Stat(sandboxenv.Path(dest))
				if err != nil {
					return err
				}
				if err := os.Chmod(sandboxenv.Path(dest), st.Mode().Perm()|0o666); err != nil {
					return err
				}
			}
		} else if !download(n, p.Versions, dest) {
			continue
		}
		if err := applyPluginConfig(n, p, neo4jConf, reference); err != nil {
			return err
		}
	}
	return nil
}

// settingsPart is text without its comments.
func settingsPart(text string) string {
	var b strings.Builder
	for _, l := range strings.Split(text, "\n") {
		l, _, _ = strings.Cut(l, "#")
		b.WriteString(l + "\n")
	}
	return b.String()
}

func applyPluginConfig(name string, p plugin, neo4jConf, reference string) error {
	fmt.Printf("Applying default values for plugin %s to neo4j.conf\n", name)
	keys := make([]string, 0, len(p.Properties))
	for k := range p.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := p.Properties[k]
		if strings.Contains(settingsPart(reference), k+"=") {
			fmt.Printf("Skipping %s for plugin %s because it is already set.\n", k, name)
			fmt.Printf("You may need to add %s to the %s setting in your configuration file.\n", v, k)
			continue
		}
		text, err := readConf(neo4jConf)
		if err != nil {
			return err
		}
		if strings.Contains(settingsPart(text), k+"=") {
			text = strings.ReplaceAll(text, k+"=", k+"="+v+",")
			if err := os.WriteFile(sandboxenv.Path(neo4jConf), []byte(text), 0o600); err != nil {
				return err
			}
			continue
		}
		if err := appendConf(neo4jConf, "\n"+k+"="+v+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// download fetches the jar the plugin's versions file lists for this
// Neo4j into dest, and reports whether it did; a plugin it cannot fetch
// leaves Neo4j to start without it, as the official image does.
func download(name, versionsURL, dest string) bool {
	out, err := exec.Command("neo4j", "--version").Output()
	if err != nil {
		log.Printf("neo4j --version: %v", err)
		return false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		log.Printf("neo4j --version printed %q", out)
		return false
	}
	version := fields[1]
	fmt.Printf("Fetching versions.json for Plugin '%s' from %s\n", name, versionsURL)
	body, err := fetch(versionsURL)
	if err != nil {
		log.Printf("ERROR: could not query %s for plugin compatibility information.\n"+
			"    This could indicate a problem with your network or this container's network settings.\n"+
			"    Neo4j will continue to start, but \"%s\" will not be loaded.", versionsURL, name)
		return false
	}
	var versions []struct {
		Neo4j string `json:"neo4j"`
		Jar   string `json:"jar"`
	}
	if err := json.Unmarshal(body, &versions); err != nil {
		log.Printf("%s: %v", versionsURL, err)
		return false
	}
	jar, best := "", ""
	for _, v := range versions {
		if Semver(v.Neo4j, version) && (jar == "" || v.Neo4j < best) {
			jar, best = v.Jar, v.Neo4j
		}
	}
	if jar == "" {
		log.Printf("ERROR: No compatible \"%s\" plugin found for Neo4j %s %s.\n"+
			"    This can happen with the newest Neo4j versions when a compatible plugin has not yet been released.\n"+
			"    You can either use an older version of Neo4j, or continue without %s.\n"+
			"    Neo4j will continue to start, but \"%s\" will not be loaded.", name, version, os.Getenv("NEO4J_EDITION"), name, name)
		return false
	}
	fmt.Printf("Installing Plugin '%s' from %s to %s \n", name, jar, dest)
	b, err := fetch(jar)
	if err != nil {
		log.Printf("%s: %v", jar, err)
		return false
	}
	if err := os.WriteFile(sandboxenv.Path(dest), b, 0o644); err != nil {
		log.Fatal(err)
	}
	return true
}

// fetch reads url's body, trying as wget --tries 30 --timeout 300 does.
func fetch(url string) ([]byte, error) {
	client := &http.Client{Timeout: 300 * time.Second}
	var err error
	for try := 0; try < 30; try++ {
		var resp *http.Response
		resp, err = client.Get(url)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		b, rerr := io.ReadAll(bufio.NewReader(resp.Body))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		if rerr != nil {
			err = rerr
			continue
		}
		return b, nil
	}
	return nil, err
}

type semverParts struct{ major, minor, patch *string }

var semverPattern = regexp.MustCompile(`([0-9x*]+)(\.([0-9x*]+))?(\.?([0-9x*]+))?`)

func parseSemver(v string) semverParts {
	if v == "" {
		return semverParts{}
	}
	m := semverPattern.FindStringSubmatchIndex(v)
	if m == nil {
		major := v
		return semverParts{major: &major}
	}
	part := func(i int) *string {
		if m[2*i] < 0 {
			return nil
		}
		s := v[m[2*i]:m[2*i+1]]
		return &s
	}
	return semverParts{part(1), part(3), part(5)}
}

func eq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func wild(a *string) bool { return a != nil && (*a == "x" || *a == "*") }

// Semver reports whether the version spec of a plugin's versions file
// matches version, as the official image's semver.jq does.
func Semver(spec, version string) bool {
	if version == "" {
		return false
	}
	if spec == version {
		return true
	}
	s, r := parseSemver(spec), parseSemver(version)
	switch {
	case eq(s.major, r.major) && eq(s.minor, r.minor) && eq(s.patch, r.patch):
		return true
	case !eq(s.major, r.major) && !wild(s.major):
		return false
	case !eq(s.minor, r.minor) && !wild(s.minor):
		return false
	case !eq(s.patch, r.patch) && !wild(s.patch):
		return false
	case r.minor == nil && wild(s.minor):
		return false
	case r.patch == nil && wild(s.patch):
		return false
	case r.major == nil && r.minor == nil && r.patch == nil:
		return false
	}
	return true
}
