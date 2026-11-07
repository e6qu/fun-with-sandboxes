// Command entry runs Vault inside a fwsd container as the official image's
// docker-entrypoint.sh does: VAULT_LOCAL_CONFIG becomes a configuration
// file, VAULT_REDIRECT_INTERFACE and VAULT_CLUSTER_INTERFACE name the
// interfaces whose addresses Vault advertises, a server takes the image's
// configuration directory and the development root token and listen
// address the environment names, a first argument that is an option or
// one of vault's commands runs vault, and vault runs as the vault user.
// The official image gives vault the IPC_LOCK file capability; macOS has
// no file capabilities, and lets vault lock memory as it is.
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

const configDir = "/vault/config"

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	args := os.Args[1:]
	for _, v := range []struct{ env, addr, scheme, port string }{
		{"VAULT_REDIRECT_INTERFACE", "VAULT_REDIRECT_ADDR", "http", "8200"},
		{"VAULT_CLUSTER_INTERFACE", "VAULT_CLUSTER_ADDR", "https", "8201"},
	} {
		name := os.Getenv(v.env)
		if name == "" {
			continue
		}
		a, err := interfaceAddress(name)
		if err != nil {
			log.Fatalf("Could not find IP for interface '%s', exiting", name)
		}
		os.Setenv(v.addr, v.scheme+"://"+a+":"+v.port)
		log.Printf("Using %s for %s's IP: %s", name, v.addr, a)
	}
	if c := os.Getenv("VAULT_LOCAL_CONFIG"); c != "" {
		if err := os.WriteFile(sandboxenv.Path(configDir+"/local.json"), []byte(c+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = append([]string{"vault"}, args...)
	}
	switch {
	case len(args) > 0 && args[0] == "server":
		listen := os.Getenv("VAULT_DEV_LISTEN_ADDRESS")
		if listen == "" {
			listen = "0.0.0.0:8200"
		}
		server := []string{"vault", "server", "-config=" + configDir, "-dev-root-token-id=" + os.Getenv("VAULT_DEV_ROOT_TOKEN_ID"), "-dev-listen-address=" + listen}
		args = append(server, args[1:]...)
	case len(args) > 0 && args[0] == "version":
		args = append([]string{"vault"}, args...)
	case len(args) > 0 && isCommand(args[0]):
		args = append([]string{"vault"}, args...)
	}
	if len(args) == 0 {
		log.Fatal("no command to run")
	}
	if args[0] == "vault" {
		var own []string
		if os.Getenv("SKIP_CHOWN") == "" {
			own = []string{sandboxenv.Path(configDir), sandboxenv.Path("/vault/logs"), sandboxenv.Path("/vault/file")}
		}
		if err := sandboxenv.Become("vault", own...); err != nil {
			log.Fatal(err)
		}
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// isCommand reports whether vault's help lists name as a command, as the
// official entrypoint asks vault.
func isCommand(name string) bool {
	out, _ := exec.Command("vault", "--help", name).CombinedOutput()
	return strings.Contains(string(out), "vault "+name)
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
