package egress

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/roles"
)

// Role is the value of the engine's role variable that starts its binary
// as a stack's egress proxy, a process of its own so the proxy outlives the
// engine with the stack's containers.
const Role = "egress"

const roleVar = roles.Var

func init() {
	if os.Getenv(roleVar) == Role && len(os.Args) > 2 {
		os.Exit(serveRole(os.Args[len(os.Args)-1], os.Args[len(os.Args)-2]))
	}
}

// serveRole runs the proxy on addr for the stack whose network file is
// network until SIGTERM, after reporting on descriptor 3 that it listens.
func serveRole(addr, network string) int {
	ready := os.NewFile(3, "ready")
	policy := HostPolicy()
	policy.Members = network
	p, err := Listen(addr, policy)
	if err != nil {
		json.NewEncoder(ready).Encode(map[string]string{"error": err.Error()})
		return 1
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	json.NewEncoder(ready).Encode(map[string]string{})
	ready.Close()
	<-sigs
	p.Close()
	return 0
}

// Start runs a proxy process on addr for the stack whose network file is
// network, logging to log, and returns its pid once it listens.
func Start(addr, network string, log *os.File) (*os.Process, error) {
	self, err := roles.Binary()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cmd := exec.Command(self, "egress", network, addr)
	env := []string{roleVar + "=" + Role}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, roleVar+"=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, err
	}
	w.Close()
	go cmd.Wait()
	b, _ := io.ReadAll(r)
	var status map[string]string
	if err := json.Unmarshal(b, &status); err != nil {
		return nil, fmt.Errorf("egress proxy exited before it listened")
	}
	if status["error"] != "" {
		return nil, fmt.Errorf("egress proxy: %s", status["error"])
	}
	return cmd.Process, nil
}
