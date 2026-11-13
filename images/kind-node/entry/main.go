// Command entry is fws/kind-node's init, its kubeadm and cp, its ctr,
// containerd and journalctl (tools.go), and fws/k3s's k3s (k3s.go), by
// the name it runs as.
//
// As the entrypoint it does what systemd and its units do in kind's
// kindest/node: it tells kind the node is up, gives the engine fwsd nests
// in the node container the control plane's images under the names
// kubeadm writes into its static pods, and keeps the kubelet running on
// the configuration kubeadm writes, with the flags a node without cgroups
// takes, and fws-kube-proxy beside it as the node's kube-proxy. It needs
// the container to be privileged, which kind's nodes are.
//
// As kubeadm it runs upstream's, skipping the kube-proxy and CoreDNS
// addons at init: fws-kube-proxy keeps the node engine's services to the
// cluster's, and the container library resolves cluster names, so the
// Linux images those addons run have no use here.
//
// As cp it copies its standard input to a file, as kind writes files into
// a node with cp /dev/stdin FILE, and runs coreutils' cp for anything
// else: an exec's standard input is a socket, which macOS's stat of
// /dev/stdin and fstat of the descriptor that opens it name apart, so
// coreutils' cp takes it for a file replaced while it copied.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// realKubeadm is upstream's kubeadm.
	realKubeadm = "/usr/local/libexec/fws/kubeadm"
	// imagesDir holds the control plane's image archives, and imagesList
	// the names kubeadm gives them: an archive's tag, then the name.
	imagesDir  = "/kind/images"
	imagesList = "/kind/images.txt"
	// The files kubeadm writes for the kubelet, as its systemd drop-in
	// reads them.
	kubeletConfig       = "/var/lib/kubelet/config.yaml"
	kubeletFlags        = "/var/lib/kubelet/kubeadm-flags.env"
	kubeletKubeconfig   = "/etc/kubernetes/kubelet.conf"
	bootstrapKubeconfig = "/etc/kubernetes/bootstrap-kubelet.conf"
	// criSocket is where kind's kubeadm configuration has the runtime.
	criSocket = "/run/containerd/containerd.sock"
	// skipInit are the phases kubeadm init skips: kind's preflight, and
	// the addons whose Linux images a node does without.
	skipInit = "preflight,addon/kube-proxy,addon/coredns"
)

func main() {
	log.SetFlags(0)
	switch filepath.Base(os.Args[0]) {
	case "kubeadm":
		kubeadm(os.Args[1:])
	case "cp":
		cp(os.Args[1:])
	case "journalctl":
		os.Exit(journalctl(os.Args[1:], os.Stdout))
	case "containerd":
		os.Exit(containerd(os.Args[1:], os.Stdout))
	case "crictl":
		crictl(os.Args[1:])
	case "k3s":
		k3s(os.Args[1:])
	case "ctr":
		engine := os.Getenv("FWS_ENGINE")
		if engine == "" {
			engine = criSocket
		}
		os.Exit(ctr(engineClient(engine), os.Args[1:], os.Stdin, os.Stdout))
	}
	n := boot("fws-kind-node")
	defer n.stop()
	// kind waits for systemd's word before it runs commands in the node,
	// which boot gave.
	n.run()
}

// node is a booted node: its engine's socket, the output its journal
// keeps, and the context its signals end.
type node struct {
	engine string
	stdout io.Writer
	ctx    context.Context
	stop   context.CancelFunc
}

// boot readies the node: its engine at containerd's socket, its journal,
// the word kind waits for, and the control plane's images in its engine.
func boot(prefix string) *node {
	log.SetPrefix(prefix + ": ")
	engine := os.Getenv("FWS_ENGINE")
	if engine == "" {
		log.Fatal("the node needs its own engine: run it --privileged, as kind does")
	}
	// The node's output goes to its log too, which journalctl reads, as
	// systemd's journal holds it.
	journal, err := openLog(journalLog)
	if err != nil {
		log.Fatal(err)
	}
	stdout := io.MultiWriter(os.Stdout, journal)
	log.SetOutput(io.MultiWriter(os.Stderr, journal))
	if err := os.MkdirAll(filepath.Dir(criSocket), 0o755); err != nil {
		log.Fatal(err)
	}
	os.Remove(criSocket)
	if err := os.Symlink(engine, criSocket); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	fmt.Fprintln(stdout, "Reached target Multi-User System.")
	if err := loadImages(engineClient(engine)); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintln(stdout, "Loaded the control plane's images.")
	return &node{engine: engine, stdout: stdout, ctx: ctx, stop: stop}
}

// run keeps the kubelet and fws-kube-proxy running until the node stops.
func (n *node) run() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		supervise(n.ctx, "kubelet", n.stdout, kubeletReady, func() []string { return kubeletArgs(n.engine) })
	}()
	go func() {
		defer wg.Done()
		supervise(n.ctx, "fws-kube-proxy", n.stdout, func() bool { return exists(kubeletKubeconfig) }, func() []string {
			return []string{"fws-kube-proxy", "--kubeconfig=" + kubeletKubeconfig}
		})
	}()
	wg.Wait()
}

// kubeadm runs upstream's kubeadm with args, as kubeadmArgs gives them.
func kubeadm(args []string) {
	if err := syscall.Exec(realKubeadm, append([]string{"kubeadm"}, kubeadmArgs(args)...), os.Environ()); err != nil {
		log.Fatalf("kubeadm: %v", err)
	}
}

// kubeadmArgs are kubeadm's arguments args, which at init skip skipInit's
// phases as well as any they name: the flag outranks the configuration's
// skipPhases, which kind sets to preflight.
func kubeadmArgs(args []string) []string {
	if len(args) == 0 || args[0] != "init" {
		return args
	}
	skip := skipInit
	rest := []string{"init"}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--skip-phases="):
			skip += "," + strings.TrimPrefix(a, "--skip-phases=")
		case a == "--skip-phases" && i+1 < len(args):
			i++
			skip += "," + args[i]
		default:
			rest = append(rest, a)
		}
	}
	return append(rest, "--skip-phases="+skip)
}

// coreutilsCp is the cp cp runs for what it does not copy itself.
const coreutilsCp = "/bin/cp"

// cp copies standard input to the file args names, given as /dev/stdin
// FILE, and runs coreutils' cp with any other arguments.
func cp(args []string) {
	if len(args) == 2 && args[0] == "/dev/stdin" {
		if err := copyStdin(args[1]); err != nil {
			log.Fatalf("cp: %v", err)
		}
		os.Exit(0)
	}
	if err := syscall.Exec(coreutilsCp, append([]string{"cp"}, args...), os.Environ()); err != nil {
		log.Fatalf("cp: %v", err)
	}
}

// copyStdin writes standard input to dest, as cp writes a file: over an
// existing file, keeping its mode, or as a new one with mode 0644.
func copyStdin(dest string) error {
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		dest = filepath.Join(dest, "stdin")
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, os.Stdin); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// kubeletReady reports whether kubeadm has written what the kubelet runs
// with: its configuration and flags, and a kubeconfig, its own or the one
// it bootstraps its own with.
func kubeletReady() bool {
	return exists(kubeletConfig) && exists(kubeletFlags) && (exists(kubeletKubeconfig) || exists(bootstrapKubeconfig))
}

// kubeletArgs are the kubelet's arguments as kubeadm's systemd drop-in
// gives them, the runtime at the node's engine, and what a node without
// cgroups, swap accounting or a DNS server of its pods' own takes.
func kubeletArgs(engine string) []string {
	args := []string{"kubelet",
		"--bootstrap-kubeconfig=" + bootstrapKubeconfig, "--kubeconfig=" + kubeletKubeconfig,
		"--config=" + kubeletConfig}
	for _, a := range kubeadmFlags() {
		if strings.HasPrefix(a, "--container-runtime-endpoint=") {
			continue
		}
		args = append(args, a)
	}
	return append(args,
		"--container-runtime-endpoint=unix://"+engine,
		"--cgroups-per-qos=false", "--enforce-node-allocatable=", "--cgroup-driver=cgroupfs", "--cgroup-root=",
		"--fail-swap-on=false", "--eviction-hard=memory.available<10Mi", "--resolv-conf=/dev/null")
}

// kubeadmFlags are the flags kubeadm-flags.env gives the kubelet.
func kubeadmFlags() []string {
	b, err := os.ReadFile(kubeletFlags)
	if err != nil {
		return nil
	}
	return parseFlags(b)
}

// parseFlags reads KUBELET_KUBEADM_ARGS from an environment file.
func parseFlags(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || k != "KUBELET_KUBEADM_ARGS" {
			continue
		}
		out = append(out, strings.Fields(strings.Trim(v, `"'`))...)
	}
	return out
}

// openLog opens the log name under logDir for appending.
func openLog(name string) (*os.File, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(logDir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

// supervise runs the command args gives once ready reports true, and runs
// it again a second after it exits, as systemd restarts the kubelet, until
// ctx ends. Its output goes to out and to its unit's log.
func supervise(ctx context.Context, name string, out io.Writer, ready func() bool, args func() []string) {
	unit, err := openLog(name + ".log")
	if err != nil {
		log.Printf("%s: %v", name, err)
	} else {
		defer unit.Close()
		out = io.MultiWriter(out, unit)
	}
	for ctx.Err() == nil {
		if !ready() {
			sleep(ctx, time.Second)
			continue
		}
		a := args()
		cmd := exec.Command(a[0], a[1:]...)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			log.Printf("%s: %v", name, err)
			sleep(ctx, time.Second)
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			log.Printf("%s exited: %v", name, err)
			sleep(ctx, time.Second)
		case <-ctx.Done():
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

type engineAPI struct{ c *http.Client }

func engineClient(sock string) engineAPI {
	return engineAPI{&http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

func (a engineAPI) post(path string, body io.Reader) error {
	res, err := a.c.Post("http://engine"+path, "application/x-tar", body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s: %s", path, res.Status, bytes.TrimSpace(b))
	}
	// A load reports a failure in its stream, with a 200.
	if bytes.Contains(b, []byte(`"errorDetail"`)) || bytes.Contains(b, []byte(`"error"`)) {
		return fmt.Errorf("%s: %s", path, bytes.TrimSpace(b))
	}
	return nil
}

// loadImages loads the archives under imagesDir into the node's engine
// and gives each the names imagesList lists for its tag.
func loadImages(api engineAPI) error {
	archives, err := filepath.Glob(filepath.Join(imagesDir, "*.tar"))
	if err != nil {
		return err
	}
	// The archives load at once, as a load spends its time unpacking one
	// layer at a time: a node is ready in the time of its largest.
	errs := make(chan error, len(archives))
	for _, p := range archives {
		go func() {
			f, err := os.Open(p)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			if err := api.post("/images/load?quiet=1", f); err != nil {
				errs <- fmt.Errorf("loading %s: %w", p, err)
				return
			}
			errs <- nil
		}()
	}
	for range archives {
		if e := <-errs; e != nil && err == nil {
			err = e
		}
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(imagesList)
	if err != nil {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		repo, tag := splitRef(f[1])
		q := url.Values{"repo": {repo}, "tag": {tag}}
		if err := api.post("/images/"+f[0]+"/tag?"+q.Encode(), nil); err != nil {
			return fmt.Errorf("naming %s %s: %w", f[0], f[1], err)
		}
	}
	return nil
}
