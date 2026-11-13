package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The node has no containerd and no systemd; these stand in for the
// commands kind runs against them in a node: ctr to load and name images,
// containerd to report its configuration, and journalctl to read the logs
// of the units it would run. The images go to the engine fwsd nests in the
// node, which the kubelet runs pods on; crictl is upstream's, run against
// that engine's CRI.

const (
	// logDir holds the logs journalctl reads: the node's whole output, and
	// each supervised command's.
	logDir     = "/var/log"
	journalLog = "fws-kind-node.log"
)

// unitLog is the log journalctl reads for unit, as systemd names it.
func unitLog(unit string) string {
	unit = strings.TrimSuffix(unit, ".service")
	if unit == "" {
		return filepath.Join(logDir, journalLog)
	}
	return filepath.Join(logDir, unit+".log")
}

// journalctl prints the log of the unit -u names, or the node's whole log,
// as journalctl --no-pager does; a unit the node does not run has none.
func journalctl(args []string, out io.Writer) int {
	unit := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unit":
			if i+1 < len(args) {
				i++
				unit = args[i]
			}
		case strings.HasPrefix(a, "--unit="):
			unit = strings.TrimPrefix(a, "--unit=")
		case strings.HasPrefix(a, "-u") && len(a) > 2:
			unit = a[2:]
		}
	}
	f, err := os.Open(unitLog(unit))
	if err != nil {
		fmt.Fprintln(out, "-- No entries --")
		return 0
	}
	defer f.Close()
	io.Copy(out, f)
	return 0
}

// realCrictl is upstream's crictl.
const realCrictl = "/usr/local/libexec/fws/crictl"

// crictl runs upstream's crictl against the node's engine, whose CRI the
// kubelet uses, unless the command names an endpoint of its own.
func crictl(args []string) {
	if err := syscall.Exec(realCrictl, append([]string{"crictl"}, crictlArgs(os.Getenv("FWS_ENGINE"), args)...), os.Environ()); err != nil {
		log.Fatalf("crictl: %v", err)
	}
}

// crictlArgs are crictl's arguments args, with the runtime and image
// endpoints at engine's socket before them where args name neither.
func crictlArgs(engine string, args []string) []string {
	if engine == "" {
		return args
	}
	for _, a := range args {
		for _, f := range []string{"--runtime-endpoint", "-r", "--image-endpoint", "-i"} {
			if a == f || strings.HasPrefix(a, f+"=") {
				return args
			}
		}
	}
	return append([]string{"--runtime-endpoint=unix://" + engine, "--image-endpoint=unix://" + engine}, args...)
}

// containerdConfig is what containerd config dump prints: kind reads the
// snapshotter it loads images with from it.
const containerdConfig = `version = 2

# fws/kind-node runs no containerd: its runtime is the engine fwsd nests in
# the node, which serves the CRI at /run/containerd/containerd.sock.
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "native"
`

// containerd answers config dump and version; the node runs no daemon.
func containerd(args []string, out io.Writer) int {
	switch {
	case len(args) >= 2 && args[0] == "config" && args[1] == "dump":
		fmt.Fprint(out, containerdConfig)
		return 0
	case len(args) == 1 && (args[0] == "--version" || args[0] == "-v"):
		fmt.Fprintln(out, "containerd fws-kind-node (the node's runtime is the engine fwsd nests in it)")
		return 0
	}
	log.Printf("containerd %s: fws/kind-node runs no containerd; its runtime is the engine fwsd nests in the node", strings.Join(args, " "))
	return 1
}

// ctr serves the image commands kind runs: images import, which loads an
// archive into the node's engine, images tag, and images ls. Global flags
// such as --namespace name containerd's namespaces, which the engine has
// none of.
func ctr(api engineAPI, args []string, stdin io.Reader, out io.Writer) int {
	args = withoutFlags(args, true)
	if len(args) < 2 || (args[0] != "images" && args[0] != "image" && args[0] != "i") {
		log.Printf("ctr %s: fws/kind-node's ctr serves images import, tag and ls", strings.Join(args, " "))
		return 1
	}
	rest := args[2:]
	switch args[1] {
	case "import":
		operands := withoutFlags(rest, false)
		if len(operands) != 1 {
			log.Print("ctr images import: name one archive, or - for standard input")
			return 1
		}
		var r io.Reader = stdin
		if operands[0] != "-" {
			f, err := os.Open(operands[0])
			if err != nil {
				log.Printf("ctr images import: %v", err)
				return 1
			}
			defer f.Close()
			r = f
		}
		if err := api.post("/images/load?quiet=1", r); err != nil {
			log.Printf("ctr images import: %v", err)
			return 1
		}
		return 0
	case "tag":
		operands := withoutFlags(rest, false)
		if len(operands) < 2 {
			log.Print("ctr images tag: name the image and its new names")
			return 1
		}
		for _, name := range operands[1:] {
			repo, tag := splitRef(name)
			q := url.Values{"repo": {repo}, "tag": {tag}}
			if err := api.post("/images/"+operands[0]+"/tag?"+q.Encode(), nil); err != nil {
				log.Printf("ctr images tag: %v", err)
				return 1
			}
			fmt.Fprintln(out, name)
		}
		return 0
	case "ls", "list":
		res, err := api.c.Get("http://engine/images/json")
		if err != nil {
			log.Printf("ctr images ls: %v", err)
			return 1
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			log.Printf("ctr images ls: %s", res.Status)
			return 1
		}
		var imgs []struct {
			ID       string `json:"Id"`
			RepoTags []string
			Size     int64
		}
		if err := json.NewDecoder(res.Body).Decode(&imgs); err != nil {
			log.Printf("ctr images ls: %v", err)
			return 1
		}
		fmt.Fprintln(out, "REF\tDIGEST\tSIZE")
		for _, img := range imgs {
			for _, t := range img.RepoTags {
				fmt.Fprintf(out, "%s\t%s\t%d\n", t, img.ID, img.Size)
			}
		}
		return 0
	}
	log.Printf("ctr images %s: fws/kind-node's ctr serves images import, tag and ls", args[1])
	return 1
}

// withoutFlags drops args' flags: the leading ones alone with leading set,
// ctr's global flags, else every one. A flag takes its value after =, but
// for --namespace, -n and --address, which take the next argument.
func withoutFlags(args []string, leading bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			if leading {
				return append(out, args[i:]...)
			}
			out = append(out, a)
			continue
		}
		switch a {
		case "--namespace", "-n", "--address", "-a":
			i++
		}
	}
	return out
}

// splitRef splits an image reference into its repository and tag, latest
// where it names none.
func splitRef(ref string) (repo, tag string) {
	repo, tag = ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	return repo, tag
}
