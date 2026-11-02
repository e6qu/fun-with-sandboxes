// Command entry runs nginx inside a fwsd container. It renders the
// /etc/nginx/templates/*.template files into /etc/nginx/conf.d, substituting
// environment variables as the official image's entrypoint does, and a main
// configuration that listens on the port the engine assigned to port 80,
// keeps every path nginx writes inside the container, and serves
// /usr/share/nginx/html unless a conf.d file defines servers.
package main

import (
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
	port, err := sandboxenv.Port("80")
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		c := http.Client{Timeout: 2 * time.Second}
		res, err := c.Get("http://127.0.0.1:" + port + "/")
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
	conf, err := render(pkg, port)
	if err != nil {
		log.Fatal(err)
	}
	args := []string{"nginx", "-p", sandboxenv.Path("/var/lib/nginx"), "-c", conf, "-e", "stderr"}
	if len(os.Args) > 1 && os.Args[1] != "nginx" {
		log.Fatalf("unknown command %q: the image runs nginx", os.Args[1])
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// render writes conf.d from the templates and the main configuration, and
// returns the main configuration's path.
func render(pkg, port string) (string, error) {
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
		if err := os.WriteFile(out, []byte(os.Expand(string(b), os.Getenv)), 0o644); err != nil {
			return "", err
		}
		log.Printf("rendered %s", out)
	}
	servers := ""
	if conf, _ := filepath.Glob(filepath.Join(confd, "*.conf")); len(conf) == 0 {
		servers = fmt.Sprintf(`    server {
        listen 127.0.0.1:%s;
        root %s;
        index index.html index.htm;
    }
`, port, sandboxenv.Path("/usr/share/nginx/html"))
	}
	var temps strings.Builder
	for _, t := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		fmt.Fprintf(&temps, "    %s_temp_path %s;\n", t, filepath.Join(state, t))
	}
	main := fmt.Sprintf(`daemon off;
worker_processes 1;
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
