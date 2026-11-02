// Command probe reports what a process sees through the container library:
// each argument names a check, and the probe prints its result.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"syscall"
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
