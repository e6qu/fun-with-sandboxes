// Command entry runs SeaweedFS inside a fwsd container as the official
// image's entrypoint does: as the seaweed user, with FIPS 140-3 mode on,
// and with each subcommand's defaults for the /data volume.
package main

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

const weed = "/bin/weed"

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	if g := os.Getenv("GODEBUG"); g != "" {
		os.Setenv("GODEBUG", g+",fips140=on")
	} else {
		os.Setenv("GODEBUG", "fips140=on")
	}
	if err := sandboxenv.Become("seaweed", sandboxenv.Path("/data")); err != nil {
		log.Fatal(err)
	}
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var argv []string
	switch cmd {
	case "master":
		argv = append([]string{"-logtostderr=true", "master", "-mdir=/data", "-volumeSizeLimitMB=1024"}, args...)
	case "volume":
		def := []string{"-dir=/data", "-max=0"}
		if passed("-max", args) {
			def = def[:1]
		}
		argv = append(append([]string{"-logtostderr=true", "volume"}, def...), args...)
	case "server":
		def := []string{"-dir=/data", "-volume.max=0", "-master.volumeSizeLimitMB=1024"}
		if passed("-volume.max", args) {
			def = []string{"-dir=/data", "-master.volumeSizeLimitMB=1024"}
		}
		argv = append(append([]string{"-logtostderr=true", "server"}, def...), args...)
	case "mini":
		def := []string{"-dir=/data"}
		if passed("-dir", args) {
			def = nil
		}
		argv = append(append([]string{"-logtostderr=true", "mini"}, def...), args...)
	case "filer":
		argv = append([]string{"-logtostderr=true", "filer"}, args...)
	case "s3":
		argv = append([]string{"-logtostderr=true", "s3", "-domainName=" + os.Getenv("S3_DOMAIN_NAME"),
			"-key.file=" + os.Getenv("S3_KEY_FILE"), "-cert.file=" + os.Getenv("S3_CERT_FILE")}, args...)
	case "shell":
		shell := exec.Command(weed, "-logtostderr=true", "shell", "-cluster="+os.Getenv("SHELL_CLUSTER"), "-filer="+os.Getenv("SHELL_FILER"),
			"-filerGroup="+os.Getenv("SHELL_FILER_GROUP"), "-master="+os.Getenv("SHELL_MASTER"), "-options="+os.Getenv("SHELL_OPTIONS"))
		shell.Stdin = strings.NewReader(strings.Join(args, " ") + "\n")
		shell.Stdout, shell.Stderr = os.Stdout, os.Stderr
		if err := shell.Run(); err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				os.Exit(e.ExitCode())
			}
			log.Fatal(err)
		}
		return
	default:
		if cmd != "" {
			argv = append([]string{cmd}, args...)
		}
	}
	log.Fatal(syscall.Exec(sandboxenv.Path(weed), append([]string{"weed"}, argv...), os.Environ()))
}

// passed reports whether args set flag, as -flag or --flag, alone or with
// a value after =.
func passed(flag string, args []string) bool {
	for _, a := range args {
		for _, f := range []string{flag, "-" + flag} {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}
