// Command entry runs Consul inside a fwsd container as the official
// image's docker-entrypoint.sh does: CONSUL_LOCAL_CONFIG becomes a
// configuration file, an agent takes the image's data and configuration
// directories and the addresses of the interfaces CONSUL_BIND_INTERFACE
// and CONSUL_CLIENT_INTERFACE name, a first argument that is an option or
// one of consul's commands runs consul, and consul runs as the consul
// user.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

const (
	dataDir   = "/consul/data"
	configDir = "/consul/config"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	args := os.Args[1:]
	var bind, client []string
	for _, v := range []struct {
		env, flag string
		out       *[]string
	}{{"CONSUL_BIND_INTERFACE", "-bind", &bind}, {"CONSUL_CLIENT_INTERFACE", "-client", &client}} {
		name := os.Getenv(v.env)
		if name == "" {
			continue
		}
		a, err := interfaceAddress(name)
		if err != nil {
			log.Fatalf("Could not find IP for interface '%s', exiting", name)
		}
		log.Printf("==> Found address '%s' for interface '%s', setting %s option...", a, name, strings.TrimPrefix(v.flag, "-"))
		*v.out = []string{v.flag + "=" + a}
	}
	if c := os.Getenv("CONSUL_LOCAL_CONFIG"); c != "" {
		if err := os.WriteFile(sandboxenv.Path(configDir+"/local.json"), []byte(c+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = append([]string{"consul"}, args...)
	}
	switch {
	case len(args) > 0 && args[0] == "agent":
		agent := []string{"consul", "agent", "-data-dir=" + dataDir, "-config-dir=" + configDir}
		args = append(append(append(agent, bind...), client...), args[1:]...)
	case len(args) > 0 && args[0] == "version":
		args = append([]string{"consul"}, args...)
	case len(args) > 0 && isCommand(args[0]):
		args = append([]string{"consul"}, args...)
	}
	if len(args) == 0 {
		log.Fatal("no command to run")
	}
	if args[0] == "consul" {
		if _, set := os.LookupEnv("CONSUL_DISABLE_PERM_MGMT"); !set {
			if err := sandboxenv.Become("consul", sandboxenv.Path(dataDir), sandboxenv.Path(configDir)); err != nil {
				log.Fatal(err)
			}
		}
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// isCommand reports whether consul's help lists name as a command, as the
// official entrypoint asks consul.
func isCommand(name string) bool {
	out, _ := exec.Command("consul", "--help", name).CombinedOutput()
	return strings.Contains(string(out), "consul "+name)
}

// interfaceAddress is the first IPv4 address of the interface name.
func interfaceAddress(name string) (string, error) {
	i, err := net.InterfaceByName(name)
	if err != nil {
		return "", err
	}
	addrs, err := i.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String(), nil
		}
	}
	return "", fmt.Errorf("%s has no IPv4 address", name)
}
