// Command entry runs Gitea inside a fwsd container. It writes app.ini on
// first start, with every path under /data and its HTTP and SSH servers on
// the container's ports 3000 and 22, which the container library puts on
// their host ports, and applies GITEA__ variables
// on each start, as the official image's environment-to-ini does.
package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	if len(os.Args) > 1 && os.Args[1] == "health" {
		c := http.Client{Timeout: 2 * time.Second}
		res, err := c.Get("http://127.0.0.1:3000/api/healthz")
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			log.Printf("healthz: %s", res.Status)
			os.Exit(1)
		}
		return
	}
	data := sandboxenv.Path("/data/gitea")
	conf := filepath.Join(data, "conf", "app.ini")
	for _, d := range []string{filepath.Dir(conf), sandboxenv.Path("/data/git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	if err := sandboxenv.Become("git", sandboxenv.Path("/data")); err != nil {
		log.Fatal(err)
	}
	ini, err := readINI(conf)
	if err != nil {
		log.Fatal(err)
	}
	if len(ini.order) == 0 {
		defaults(ini, data)
	}
	applyEnv(ini, os.Environ())
	if err := ini.write(conf); err != nil {
		log.Fatal(err)
	}

	// gitea on /bin is a wrapper script that names its programs by store
	// path; the binary beside it runs with the image's own.
	wrapper, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/gitea"))
	if err != nil {
		log.Fatal(err)
	}
	bin := filepath.Join(filepath.Dir(wrapper), ".gitea-wrapped")
	git, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/git"))
	if err != nil {
		log.Fatal(err)
	}
	gitPkg := filepath.Dir(filepath.Dir(git))
	env := append(os.Environ(),
		"GITEA_WORK_DIR="+data,
		"GITEA_CUSTOM="+filepath.Join(data, "custom"),
		"GIT_EXEC_PATH="+filepath.Join(gitPkg, "libexec", "git-core"),
		"GIT_TEMPLATE_DIR="+filepath.Join(gitPkg, "share", "git-core", "templates"),
		"HOME="+sandboxenv.Path("/data/git"),
	)
	args := []string{"gitea", "web", "--config", conf, "--work-path", data}
	if len(os.Args) > 1 && os.Args[1] != "web" {
		args = append([]string{"gitea"}, os.Args[1:]...)
		args = append(args, "--config", conf, "--work-path", data)
	}
	log.Fatal(syscall.Exec(bin, args, env))
}

func defaults(ini *iniFile, data string) {
	ini.set("", "APP_NAME", "Gitea: Git with a cup of tea")
	ini.set("", "RUN_MODE", "prod")
	ini.set("", "WORK_PATH", data)
	ini.set("server", "HTTP_ADDR", "127.0.0.1")
	ini.set("server", "HTTP_PORT", "3000")
	ini.set("server", "DOMAIN", "localhost")
	ini.set("server", "ROOT_URL", "http://localhost:3000/")
	ini.set("server", "APP_DATA_PATH", data)
	ini.set("server", "STATIC_ROOT_PATH", sandboxenv.Path(os.Getenv("GITEA_DATA")))
	ini.set("server", "DISABLE_SSH", "false")
	ini.set("server", "START_SSH_SERVER", "true")
	ini.set("server", "SSH_LISTEN_HOST", "127.0.0.1")
	ini.set("server", "SSH_LISTEN_PORT", "22")
	ini.set("server", "SSH_PORT", "22")
	ini.set("server", "LFS_START_SERVER", "true")
	ini.set("database", "DB_TYPE", "sqlite3")
	ini.set("database", "PATH", filepath.Join(data, "gitea.db"))
	ini.set("repository", "ROOT", sandboxenv.Path("/data/git/repositories"))
	ini.set("log", "MODE", "console")
	ini.set("log", "ROOT_PATH", filepath.Join(data, "log"))
	ini.set("security", "INSTALL_LOCK", "false")
}

// applyEnv sets GITEA__SECTION__KEY variables, as environment-to-ini reads
// them: DEFAULT names the root section, and _0X2E_ and _0X2D_ stand for a
// dot and a hyphen.
func applyEnv(ini *iniFile, env []string) {
	sort.Strings(env)
	dec := strings.NewReplacer("_0X2E_", ".", "_0X2D_", "-")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		rest, ok := strings.CutPrefix(k, "GITEA__")
		if !ok {
			continue
		}
		section, key, ok := strings.Cut(rest, "__")
		if !ok {
			continue
		}
		section = strings.ToLower(dec.Replace(section))
		if section == "default" {
			section = ""
		}
		ini.set(section, strings.ToUpper(dec.Replace(key)), v)
	}
}

// iniFile keeps an INI file's sections and keys in their order.
type iniFile struct {
	order []string
	keys  map[string][]string
	vals  map[string]map[string]string
}

func readINI(path string) (*iniFile, error) {
	ini := &iniFile{keys: map[string][]string{}, vals: map[string]map[string]string{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return ini, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSpace(line[1 : len(line)-1])
			ini.section(section)
		default:
			k, v, _ := strings.Cut(line, "=")
			ini.set(section, strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return ini, sc.Err()
}

func (i *iniFile) section(s string) {
	if _, ok := i.vals[s]; !ok {
		i.vals[s] = map[string]string{}
		i.order = append(i.order, s)
	}
}

func (i *iniFile) set(section, key, value string) {
	i.section(section)
	if _, ok := i.vals[section][key]; !ok {
		i.keys[section] = append(i.keys[section], key)
	}
	i.vals[section][key] = value
}

func (i *iniFile) write(path string) error {
	var b strings.Builder
	sections := append([]string{}, i.order...)
	sort.SliceStable(sections, func(a, c int) bool { return sections[a] == "" && sections[c] != "" })
	for _, s := range sections {
		if s != "" {
			fmt.Fprintf(&b, "\n[%s]\n", s)
		}
		for _, k := range i.keys[s] {
			fmt.Fprintf(&b, "%s = %s\n", k, i.vals[s][k])
		}
	}
	return os.WriteFile(path, []byte(strings.TrimPrefix(b.String(), "\n")), 0o600)
}
