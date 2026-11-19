package pathmap

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/members"
	"github.com/e6qu/fun-with-sandboxes/internal/router"
)

// v6ClientSource connects to the IPv6 address and port its arguments
// name, as a client in a container does, and prints what it reads.
const v6ClientSource = `#include <arpa/inet.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>
int main(int argc, char **argv) {
    struct sockaddr_in6 a = {0};
    a.sin6_len = sizeof a;
    a.sin6_family = AF_INET6;
    a.sin6_port = htons((unsigned short)atoi(argv[2]));
    if (inet_pton(AF_INET6, argv[1], &a.sin6_addr) != 1) return 2;
    int s = socket(AF_INET6, SOCK_STREAM, 0);
    if (connect(s, (struct sockaddr *)&a, sizeof a) != 0) {
        perror("connect");
        return 1;
    }
    char buf[64];
    ssize_t n = read(s, buf, sizeof buf - 1);
    printf("%.*s\n", n > 0 ? (int)n : 0, buf);
    return 0;
}
`

// A container on an IPv6-only network reaches a pod of another node on
// it at the pod's IPv6 address: the network file lists the pod under a key
// in the member block (package engine, peerKeys), which the library
// routes the address by, through the container's own router, to the
// pod's host port.
func TestPodOfAnotherNodeAtItsIPv6Address(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, "v6client")
	if err := os.WriteFile(bin+".c", []byte(v6ClientSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-o", bin, bin+".c").CombinedOutput(); err != nil {
		t.Fatalf("build the client: %v\n%s", err, out)
	}
	pod, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pod.Close() })
	go func() {
		for {
			c, err := pod.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("from node A"))
			c.Close()
		}
	}()
	// A Unix socket's path is capped at 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "fwsp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, _ = filepath.EvalSymlinks(dir)
	network := filepath.Join(work, "network")
	ms := []members.Member{
		{Address: "127.88.0.2", Names: []string{"app"}, Networks: []string{"pods"}, Only6: []string{"pods"},
			Subnets: []members.NetAddress{{Network: "pods", Address: netip.MustParsePrefix("fd00:244:0:2::5/64")}}},
		{Address: "127.88.255.254", Ports: map[string]int{"80/tcp": pod.Addr().(*net.TCPAddr).Port}, Networks: []string{"pods"}, Only6: []string{"pods"},
			Foreign: true, Stack: "peer-node-a", Subnets: []members.NetAddress{{Network: "pods", Address: netip.MustParsePrefix("fd00:244:0:1::5/64")}}},
	}
	if err := os.WriteFile(network, members.Format(ms), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := router.Listen("127.0.0.1:0", dir, func() (*net.UDPConn, error) { return nil, fmt.Errorf("no flows") },
		router.Network{File: network, Address: "127.88.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	lib, err := Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "fd00:244:0:1::5", "80")
	cmd.Env = []string{"DYLD_INSERT_LIBRARIES=" + lib, PortDirVar + "=" + dir, RouterVar + "=" + strconv.Itoa(r.Addr().Port),
		NetworkVar + "=" + network, AddressVar + "=127.88.0.2"}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "from node A" {
		t.Errorf("the client: %v\n%s", err, out)
	}
}
