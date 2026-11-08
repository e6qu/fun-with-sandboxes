// Command entry is the dind image's dockerd-entrypoint.sh and the CLI
// image's docker-entrypoint.sh, by the name it runs as, doing what the
// official scripts do: with no arguments, or flags alone, it runs dockerd
// on the default socket and on TCP, with TLS certificates it generates
// under DOCKER_TLS_CERTDIR; any other command runs with DOCKER_HOST set
// for the daemon, through the docker CLI when it names a docker command.
// The scripts' iptables and rootlesskit steps set up a Linux dockerd,
// which the engine fwsd nests in the container has no use for.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// certValidDays is the official script's, after mkcert's limit.
const certValidDays = 825

func main() {
	log.SetFlags(0)
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "docker-entrypoint.sh" {
		execute(cliArgs(args))
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		socket := "unix:///var/run/docker.sock"
		if os.Getuid() != 0 {
			run := os.Getenv("XDG_RUNTIME_DIR")
			if run == "" {
				run = fmt.Sprintf("/run/user/%d", os.Getuid())
			}
			socket = "unix://" + run + "/docker.sock"
		}
		if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
			socket = h
		}
		if dir := os.Getenv("DOCKER_TLS_CERTDIR"); dir != "" {
			if err := GenerateCerts(dir, sans()); err != nil {
				log.Fatal(err)
			}
			args = append([]string{"dockerd", "--host=" + socket, "--host=tcp://0.0.0.0:2376", "--tlsverify",
				"--tlscacert", dir + "/server/ca.pem", "--tlscert", dir + "/server/cert.pem", "--tlskey", dir + "/server/key.pem"}, args...)
		} else {
			args = append([]string{"dockerd", "--host=" + socket, "--host=tcp://0.0.0.0:2375"}, args...)
		}
	}
	if args[0] != "dockerd" {
		execute(cliArgs(args))
	}
	for _, dir := range []string{"/run", "/var/run"} {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasPrefix(strings.ToLower(d.Name()), "docker") && strings.HasSuffix(strings.ToLower(d.Name()), ".pid") {
				os.Remove(p)
			}
			return nil
		})
	}
	if os.Getuid() != 0 {
		log.Fatal("error: attempting to run rootless dockerd but missing 'rootlesskit' (perhaps the 'docker:dind-rootless' image variant is intended?)")
	}
	execute(args)
}

// cliArgs is what docker-entrypoint.sh runs: flags and docker commands
// through the docker CLI, with DOCKER_HOST, and the TLS variables when
// the daemon's client certificates are there, set for the daemon.
func cliArgs(args []string) []string {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = append([]string{"docker"}, args...)
	}
	if len(args) > 0 && exec.Command("docker", "help", args[0]).Run() == nil {
		args = append([]string{"docker"}, args...)
	}
	if os.Getenv("DOCKER_HOST") == "" {
		run := os.Getenv("XDG_RUNTIME_DIR")
		if run == "" {
			run = fmt.Sprintf("/run/user/%d", os.Getuid())
		}
		switch {
		case isSocket("/var/run/docker.sock"):
			os.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		case isSocket(run + "/docker.sock"):
			os.Setenv("DOCKER_HOST", "unix://"+run+"/docker.sock")
		case shouldTLS() || os.Getenv("DOCKER_TLS_VERIFY") != "":
			os.Setenv("DOCKER_HOST", "tcp://docker:2376")
		default:
			os.Setenv("DOCKER_HOST", "tcp://docker:2375")
		}
	}
	if strings.HasPrefix(os.Getenv("DOCKER_HOST"), "tcp:") && os.Getenv("DOCKER_TLS_VERIFY") == "" && os.Getenv("DOCKER_CERT_PATH") == "" && shouldTLS() {
		os.Setenv("DOCKER_TLS_VERIFY", "1")
		os.Setenv("DOCKER_CERT_PATH", os.Getenv("DOCKER_TLS_CERTDIR")+"/client")
	}
	if len(args) > 0 && args[0] == "dockerd" {
		fmt.Fprint(os.Stderr, "\n📎 Hey there!  It looks like you're trying to run a Docker daemon.\n\n"+
			"   You probably should use the \"dind\" image variant instead, something like:\n\n"+
			"     docker run --privileged --name some-docker ... docker:dind ...\n\n"+
			"   See https://hub.docker.com/_/docker/ for more documentation and usage examples.\n\n")
		time.Sleep(3 * time.Second)
	}
	return args
}

func shouldTLS() bool {
	dir := os.Getenv("DOCKER_TLS_CERTDIR")
	return dir != "" && nonEmpty(dir+"/client/ca.pem") && nonEmpty(dir+"/client/cert.pem") && nonEmpty(dir+"/client/key.pem")
}

func nonEmpty(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

func isSocket(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

func execute(args []string) {
	if len(args) == 0 {
		log.Fatal("no command to run")
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(bin, args, os.Environ()))
}

// SANs are the server certificate's names, as the official script lists
// them: the container's addresses, its hostname, docker and localhost,
// and DOCKER_TLS_SAN's entries.
type SANs struct {
	IPs   []net.IP
	DNS   []string
	Extra string
}

func sans() SANs {
	var s SANs
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				s.IPs = append(s.IPs, n.IP)
			}
		}
	}
	s.DNS = []string{"docker", "localhost"}
	if b, err := os.ReadFile("/etc/hostname"); err == nil {
		s.DNS = append(s.DNS, strings.TrimSpace(string(b)))
	}
	if h, err := os.Hostname(); err == nil {
		s.DNS = append(s.DNS, h, strings.SplitN(h, ".", 2)[0])
	}
	s.Extra = os.Getenv("DOCKER_TLS_SAN")
	return s
}

// GenerateCerts manages the TLS material under dir as the official script
// does: server certificates someone else's CA signed are verified and
// kept; otherwise a CA, kept between runs, signs a server and a client
// certificate, made afresh on each run.
func GenerateCerts(dir string, s SANs) error {
	if nonEmpty(dir+"/server/ca.pem") && nonEmpty(dir+"/server/cert.pem") && nonEmpty(dir+"/server/key.pem") && !nonEmpty(dir+"/ca/key.pem") {
		return verify(dir+"/server/ca.pem", dir+"/server/cert.pem")
	}
	var ca *x509.Certificate
	var caKey *rsa.PrivateKey
	if nonEmpty(dir+"/ca/key.pem") || !nonEmpty(dir+"/ca/cert.pem") {
		if err := os.MkdirAll(dir+"/ca", 0o755); err != nil {
			return err
		}
		key, err := privateKey(dir + "/ca/key.pem")
		if err != nil {
			return err
		}
		tmpl := template("docker:dind CA")
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign
		if ca, err = sign(tmpl, tmpl, &key.PublicKey, key, dir+"/ca/cert.pem"); err != nil {
			return err
		}
		caKey = key
	}
	if caKey == nil {
		return nil
	}
	if err := os.MkdirAll(dir+"/server", 0o755); err != nil {
		return err
	}
	key, err := privateKey(dir + "/server/key.pem")
	if err != nil {
		return err
	}
	tmpl := template("docker:dind server")
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	tmpl.IPAddresses, tmpl.DNSNames = s.IPs, s.DNS
	for _, e := range strings.Split(s.Extra, ",") {
		kind, v, _ := strings.Cut(strings.TrimSpace(e), ":")
		switch kind {
		case "IP":
			if ip := net.ParseIP(v); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			}
		case "DNS":
			tmpl.DNSNames = append(tmpl.DNSNames, v)
		}
	}
	tmpl.DNSNames = uniq(tmpl.DNSNames)
	if _, err := sign(tmpl, ca, &key.PublicKey, caKey, dir+"/server/cert.pem"); err != nil {
		return err
	}
	if err := copyFile(dir+"/ca/cert.pem", dir+"/server/ca.pem"); err != nil {
		return err
	}
	if err := verify(dir+"/server/ca.pem", dir+"/server/cert.pem"); err != nil {
		return err
	}
	if err := os.MkdirAll(dir+"/client", 0o755); err != nil {
		return err
	}
	if key, err = privateKey(dir + "/client/key.pem"); err != nil {
		return err
	}
	// Arbitrary client contexts share the client's key.
	if err := os.Chmod(dir+"/client/key.pem", 0o644); err != nil {
		return err
	}
	tmpl = template("docker:dind client")
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if _, err := sign(tmpl, ca, &key.PublicKey, caKey, dir+"/client/cert.pem"); err != nil {
		return err
	}
	if err := copyFile(dir+"/ca/cert.pem", dir+"/client/ca.pem"); err != nil {
		return err
	}
	return verify(dir+"/client/ca.pem", dir+"/client/cert.pem")
}

func template(cn string) *x509.Certificate {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	now := time.Now()
	return &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, certValidDays)}
}

// privateKey reads the RSA key at path, or makes and writes one, as
// openssl genrsa does, when it is missing or empty.
func privateKey(path string) (*rsa.PrivateKey, error) {
	if nonEmpty(path) {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: no PEM data", path)
		}
		if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			if r, ok := k.(*rsa.PrivateKey); ok {
				return r, nil
			}
			return nil, fmt.Errorf("%s: not an RSA key", path)
		}
		return x509.ParsePKCS1PrivateKey(blk.Bytes)
	}
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

func sign(tmpl, parent *x509.Certificate, pub *rsa.PublicKey, key *rsa.PrivateKey, path string) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM data", path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

// verify checks that the CA at caPath signed the certificate at path, as
// openssl verify does, and reports it as openssl does.
func verify(caPath, path string) error {
	ca, err := readCert(caPath)
	if err != nil {
		return err
	}
	c, err := readCert(path)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return errors.Join(fmt.Errorf("%s: verification failed", path), err)
	}
	fmt.Printf("%s: OK\n", path)
	return nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}

func uniq(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if v != "" && (i == 0 || v != s[i-1]) {
			out = append(out, v)
		}
	}
	return out
}
