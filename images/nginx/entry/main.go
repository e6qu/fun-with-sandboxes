// Command entry runs nginx inside a fwsd container. It renders the
// /etc/nginx/templates/*.template files into /etc/nginx/conf.d, substituting
// the environment variables that are set as the official image's entrypoint does, and a main
// configuration that keeps every path nginx writes inside the container and,
// unless a conf.d file defines servers, serves /usr/share/nginx/html on the
// container's port 80, which the container library puts on its host port.
// A main configuration at /etc/nginx/nginx.conf replaces the generated one.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		res, err := c.Get("http://127.0.0.1/")
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		res.Body.Close()
		return
	}
	bin, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/nginx"))
	if err != nil {
		log.Fatal(err)
	}
	// nginx's own files, mime.types among them, sit beside its binary in
	// its package.
	pkg := filepath.Dir(filepath.Dir(bin))
	conf, err := render(pkg)
	if err != nil {
		log.Fatal(err)
	}
	// A main configuration of the user's own, mounted or copied to
	// /etc/nginx/nginx.conf, stands instead of the generated one.
	if own := sandboxenv.Path("/etc/nginx/nginx.conf"); fileExists(own) {
		conf = own
	}
	if len(os.Args) > 1 && os.Args[1] != "nginx" {
		log.Fatalf("unknown command %q: the image runs nginx", os.Args[1])
	}
	var extra []string
	if len(os.Args) > 2 {
		extra = os.Args[2:]
	}
	log.Fatal(syscall.Exec(bin, nginxArgs(conf, extra), os.Environ()))
}

// nginxArgs is nginx's command line: its prefix, main configuration and
// error log in the container, then the arguments the container's command
// gave nginx, as the official image passes them on (-t, -T, -V, -g ...).
// nginx runs in the foreground, as the official image's command,
// nginx -g 'daemon off;', runs it, unless those arguments set -g
// themselves, which then say what nginx does.
func nginxArgs(conf string, extra []string) []string {
	args := []string{"nginx", "-p", sandboxenv.Path("/var/lib/nginx"), "-c", conf, "-e", "stderr"}
	if !slices.Contains(extra, "-g") {
		args = append(args, "-g", "daemon off;")
	}
	return append(args, extra...)
}

// substitute replaces the $NAME and ${NAME} references in a template whose
// variable is set, as the official image's envsubst of the variables it
// defines does, and leaves every other $ as written: nginx's own variables,
// $host and $uri, stay for nginx.
func substitute(t string, lookup func(string) (string, bool)) string {
	return envRef.ReplaceAllStringFunc(t, func(ref string) string {
		name := strings.Trim(ref, "${}")
		if v, ok := lookup(name); ok {
			return v
		}
		return ref
	})
}

var envRef = regexp.MustCompile(`\$(\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)`)

// render writes conf.d from the templates and the main configuration, and
// returns the main configuration's path.
func render(pkg string) (string, error) {
	confd := sandboxenv.Path("/etc/nginx/conf.d")
	state := sandboxenv.Path("/var/lib/nginx")
	for _, d := range []string{confd, state, sandboxenv.Path("/var/run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	templates, _ := filepath.Glob(filepath.Join(sandboxenv.Path("/etc/nginx/templates"), "*.template"))
	sort.Strings(templates)
	for _, t := range templates {
		b, err := os.ReadFile(t)
		if err != nil {
			return "", err
		}
		out := filepath.Join(confd, strings.TrimSuffix(filepath.Base(t), ".template"))
		if err := os.WriteFile(out, []byte(substitute(string(b), os.LookupEnv)), 0o644); err != nil {
			return "", err
		}
		log.Printf("rendered %s", out)
	}
	servers := ""
	if conf, _ := filepath.Glob(filepath.Join(confd, "*.conf")); len(conf) == 0 {
		servers = `    server {
        listen 80;
        listen [::]:80;
        server_name localhost;
        root /usr/share/nginx/html;
        index index.html index.htm;
    }
`
	}
	var temps strings.Builder
	for _, t := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		fmt.Fprintf(&temps, "    %s_temp_path %s;\n", t, filepath.Join(state, t))
	}
	main := fmt.Sprintf(`worker_processes 1;
pid %s;
error_log stderr;
events {
    worker_connections 256;
}
http {
    include %s;
    default_type application/octet-stream;
    access_log /dev/stdout;
%s    include %s/*.conf;
%s}
`, sandboxenv.Path("/var/run/nginx.pid"), filepath.Join(pkg, "conf", "mime.types"), temps.String(), confd, servers)
	path := filepath.Join(state, "nginx.conf")
	return path, os.WriteFile(path, []byte(main), 0o644)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
