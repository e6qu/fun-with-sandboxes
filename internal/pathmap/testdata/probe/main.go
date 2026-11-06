// Command probe reports what a process sees through the container library:
// each argument names a check, and the probe prints its result.
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	for _, check := range os.Args[1:] {
		fmt.Printf("%s=%s\n", check, run(check))
	}
}

func run(check string) string {
	switch check {
	case "read":
		b, err := os.ReadFile("/etc/marker")
		return result(string(b), err)
	case "system":
		_, err := os.Stat("/System/Library/CoreServices/SystemVersion.plist")
		return result("ok", err)
	case "write":
		err := os.WriteFile("/made/new", []byte("x"), 0o644)
		return result("ok", err)
	case "list":
		entries, err := os.ReadDir("/")
		if err != nil {
			return result("", err)
		}
		names := ""
		for _, e := range entries {
			names += e.Name() + ","
		}
		return names
	case "cwd":
		if err := os.Chdir("/made"); err != nil {
			return result("", err)
		}
		wd, err := os.Getwd()
		return result(wd, err)
	case "ids":
		return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	case "user":
		u, err := user.Current()
		if err != nil {
			return result("", err)
		}
		return u.Username + ":" + u.HomeDir
	case "chown":
		if err := os.Chown("/etc/marker", 1000, 50); err != nil {
			return result("", err)
		}
		fi, err := os.Stat("/etc/marker")
		if err != nil {
			return result("", err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	case "owner":
		fi, err := os.Stat("/etc/marker")
		if err != nil {
			return result("", err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	case "hostname":
		h, err := os.Hostname()
		return result(h, err)
	case "interfaces":
		ifs, err := net.Interfaces()
		if err != nil {
			return result("", err)
		}
		var out []string
		for _, i := range ifs {
			addrs, err := i.Addrs()
			if err != nil {
				return result("", err)
			}
			var v4 []string
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
					v4 = append(v4, n.String())
				}
			}
			loop := ""
			if i.Flags&net.FlagLoopback != 0 {
				loop = "loopback "
			}
			out = append(out, fmt.Sprintf("%s(%s%s %s)", i.Name, loop, strings.Join(v4, " "), i.HardwareAddr))
		}
		return strings.Join(out, ",")
	case "mount":
		fi, err := os.Lstat("/data")
		if err != nil {
			return result("", err)
		}
		if !fi.IsDir() {
			return "lstat " + fi.Mode().String()
		}
		b, err := os.ReadFile("/data/file")
		if err != nil {
			return result("", err)
		}
		real, err := filepath.EvalSymlinks("/data/file")
		if err != nil {
			return result("", err)
		}
		if err := os.Chdir("/data"); err != nil {
			return result("", err)
		}
		wd, err := os.Getwd()
		return result(string(b)+","+real+","+wd, err)
	case "mountat":
		// As MariaDB opens a table: a component at a time, following no
		// symlink, from /.
		fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return result("", err)
		}
		dfd, err := unix.Openat(fd, "data", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return result("", err)
		}
		ffd, err := unix.Openat(dfd, "file", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
		unix.Close(dfd)
		if err != nil {
			return result("", err)
		}
		f := os.NewFile(uintptr(ffd), "file")
		defer f.Close()
		b := make([]byte, 64)
		n, err := f.Read(b)
		return result(string(b[:n]), err)
	case "rootpath":
		// A path through $SANDBOX_ROOT resolves on the host, as a program
		// configuring itself from the container contract resolves it.
		p, err := filepath.EvalSymlinks(filepath.Join(os.Getenv("SANDBOX_ROOT"), "data", "file"))
		return result(p, err)
	case "envscript":
		out, err := exec.Command("/bin/run").Output()
		return result(string(out), err)
	case "drop":
		// As an entrypoint drops root with gosu: the program it executes
		// runs as the user it switched to.
		if err := syscall.Setgid(50); err != nil {
			return result("", err)
		}
		if err := syscall.Setuid(1000); err != nil {
			return result("", err)
		}
		cmd := exec.Command("/bin/probe", "ids")
		cmd.Env = []string{"PATH=/bin"}
		out, err := cmd.Output()
		return result(string(out), err)
	case "abslink":
		// An image's absolute links name the image's files, as under chroot.
		var parts []string
		for _, p := range []string{"/usr/bin/link", "/lib/bin/real"} {
			b, err := os.ReadFile(p)
			parts = append(parts, result(string(b), err))
		}
		target, err := os.Readlink("/usr/bin/link")
		parts = append(parts, result(target, err))
		real, err := filepath.EvalSymlinks("/lib/bin/real")
		parts = append(parts, result(real, err))
		fi, err := os.Lstat("/usr/bin/link")
		kind := "not a symlink"
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		parts = append(parts, result(kind, err))
		err = os.Chdir("/usr/bin")
		b, rerr := os.ReadFile("link")
		if err == nil {
			err = rerr
		}
		parts = append(parts, result(string(b), err))
		// .. stops at the container's /.
		for _, p := range []string{"/../../etc/marker", "/etc/up/etc/marker"} {
			b, err := os.ReadFile(p)
			parts = append(parts, result(string(b), err))
		}
		return strings.Join(parts, ",")
	case "empty":
		// An empty path names nothing, wherever the process runs.
		_, err := os.Stat("")
		return result("found", err)
	case "exec":
		out, err := exec.Command("/bin/probe", "read").Output()
		return result(string(out), err)
	}
	return "unknown check"
}

func result(v string, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return v
}
