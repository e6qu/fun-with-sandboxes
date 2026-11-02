//go:build e2e && darwin

package e2e

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A build step with an SSH mount reaches the client's SSH agent, as
// docker buildx build --ssh forwards it.
func TestBuildKitSSHMount(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if out, err := exec.Command("/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "fws-e2e-key", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	// macOS caps socket paths at 104 bytes; a test's temporary directory
	// leaves too few.
	sockDir, err := os.MkdirTemp("/tmp", "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentCmd := exec.CommandContext(ctx, "/usr/bin/ssh-agent", "-D", "-a", sock)
	announce, err := agentCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agentCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer agentCmd.Wait()
	// The agent prints its environment once its socket listens.
	if _, err := bufio.NewReader(announce).ReadString('\n'); err != nil {
		t.Fatalf("ssh-agent did not start: %v", err)
	}
	add := exec.Command("/usr/bin/ssh-add", key)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM fws/toolbox:1
RUN --mount=type=ssh ["/toolbox", "sshagent"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("docker", "buildx", "build", "--no-cache", "--progress", "plain", "--ssh", "default="+sock, "-t", "e2e/ssh:1", "--load", dir)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("docker buildx build --ssh: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/ssh:1")
	if !strings.Contains(string(out), "identities 1") || !strings.Contains(string(out), "fws-e2e-key") {
		t.Fatalf("the build step did not list the agent's key:\n%s", out)
	}
}
