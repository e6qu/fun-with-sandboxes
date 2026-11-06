package pidns

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// call makes a request of the service at sock as the container library
// does, and returns the result, errno and data.
func call(t *testing.T, sock string, op, pid, a, size int32, b uint64) (int32, syscall.Errno, []byte) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := make([]byte, RequestSize)
	binary.NativeEndian.PutUint32(req[0:], uint32(op))
	binary.NativeEndian.PutUint32(req[4:], uint32(pid))
	binary.NativeEndian.PutUint32(req[8:], uint32(a))
	binary.NativeEndian.PutUint32(req[12:], uint32(size))
	binary.NativeEndian.PutUint64(req[16:], b)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	rep, err := io.ReadAll(c)
	if err != nil || len(rep) < ReplySize {
		t.Fatalf("reply %x, %v", rep, err)
	}
	return int32(binary.NativeEndian.Uint32(rep)), syscall.Errno(binary.NativeEndian.Uint32(rep[4:])), rep[ReplySize:]
}

// startTagged starts a process holding tag in its environment.
func startTagged(t *testing.T, tag string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "600")
	cmd.Env = []string{tag}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// The service lists, inspects and signals the processes of the namespace's
// containers, and no other.
func TestServe(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pidns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	owner := startTagged(t, "SANDBOX_ROOT="+dir+"/owner")
	joiner := startTagged(t, "SANDBOX_ROOT="+dir+"/joiner")
	outsider := startTagged(t, "SANDBOX_ROOT="+dir+"/outsider")
	file := filepath.Join(dir, "ns")
	if err := Write(file, []string{"SANDBOX_ROOT=" + dir + "/owner", "SANDBOX_ROOT=" + dir + "/joiner"}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "procs")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go Serve(l, file)

	n, _, data := call(t, sock, OpList, 0, 0, 0, 0)
	listed := map[int]bool{}
	for i := 0; i+4 <= len(data); i += 4 {
		listed[int(int32(binary.NativeEndian.Uint32(data[i:])))] = true
	}
	if int(n) != len(listed) || !listed[owner.Process.Pid] || !listed[joiner.Process.Pid] || listed[outsider.Process.Pid] {
		t.Errorf("listed %v (%d), want the owner's %d and the joiner's %d, not the outsider's %d", listed, n, owner.Process.Pid, joiner.Process.Pid, outsider.Process.Pid)
	}

	const procPIDTBSDInfo, bsdInfoSize = 3, 136
	if n, errno, data := call(t, sock, OpInfo, int32(owner.Process.Pid), procPIDTBSDInfo, bsdInfoSize, 0); n != bsdInfoSize || len(data) != bsdInfoSize {
		t.Errorf("proc_pidinfo of the owner: %d, %v, %d bytes", n, errno, len(data))
	}
	if n, errno, data := call(t, sock, OpPath, int32(owner.Process.Pid), 0, 0, 0); n <= 0 || string(data) != "/bin/sleep" {
		t.Errorf("proc_pidpath of the owner: %d, %v, %q", n, errno, data)
	}
	if n, errno, _ := call(t, sock, OpArgs, int32(owner.Process.Pid), 0, 0, 0); n <= 0 {
		t.Errorf("procargs of the owner: %d, %v", n, errno)
	}
	if n, errno, _ := call(t, sock, OpInfo, int32(outsider.Process.Pid), procPIDTBSDInfo, bsdInfoSize, 0); n != -1 || errno != syscall.ESRCH {
		t.Errorf("proc_pidinfo of the outsider: %d, %v, want ESRCH", n, errno)
	}
	if n, errno, _ := call(t, sock, OpKill, int32(outsider.Process.Pid), int32(syscall.SIGTERM), 0, 0); n != -1 || errno != syscall.ESRCH {
		t.Errorf("a signal to the outsider: %d, %v, want ESRCH", n, errno)
	}
	if n, errno, _ := call(t, sock, OpKill, int32(joiner.Process.Pid), int32(syscall.SIGTERM), 0, 0); n != 0 {
		t.Fatalf("a signal to the joiner: %d, %v", n, errno)
	}
	err = joiner.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Errorf("the joiner ended with %v, want SIGTERM", err)
	}
	// -1 reaches every process of the namespace but the caller and pid 1.
	if n, errno, _ := call(t, sock, OpKill, -1, int32(syscall.SIGTERM), int32(owner.Process.Pid), uint64(os.Getpid())); n != -1 || errno != syscall.ESRCH {
		t.Errorf("a signal to all but pid 1, with only pid 1 left: %d, %v, want ESRCH", n, errno)
	}
	if err := outsider.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the outsider ended: %v", err)
	}
}
