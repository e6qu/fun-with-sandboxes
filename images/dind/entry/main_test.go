package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The certificates GenerateCerts makes let a client with the client's
// certificate and the server verify each other, over the names given.
func TestGenerateCerts(t *testing.T) {
	dir := t.TempDir()
	s := SANs{IPs: []net.IP{net.ParseIP("127.0.0.1")}, DNS: []string{"localhost", "docker", "docker"}, Extra: "DNS:dind.example,IP:10.1.2.3"}
	if err := GenerateCerts(dir, s); err != nil {
		t.Fatal(err)
	}
	caKey, _ := os.ReadFile(filepath.Join(dir, "ca", "key.pem"))
	if st, err := os.Stat(filepath.Join(dir, "client", "key.pem")); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("the client key: %v, %v", st, err)
	}
	server, err := tls.LoadX509KeyPair(filepath.Join(dir, "server", "cert.pem"), filepath.Join(dir, "server", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := tls.LoadX509KeyPair(filepath.Join(dir, "client", "cert.pem"), filepath.Join(dir, "client", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	ca, _ := os.ReadFile(filepath.Join(dir, "server", "ca.pem"))
	pool.AppendCertsFromPEM(ca)
	leaf, _ := x509.ParseCertificate(server.Certificate[0])
	if err := leaf.VerifyHostname("dind.example"); err != nil {
		t.Errorf("the server certificate: %v", err)
	}
	if err := leaf.VerifyHostname("10.1.2.3"); err != nil {
		t.Errorf("the server certificate: %v", err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{server}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	c, err := tls.Dial("tcp", l.Addr().String(), &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{client}, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "ok" {
		t.Errorf("over TLS: %q, %v", buf, err)
	}
	c.Close()
	// A second run keeps the CA's key and makes the certificates afresh.
	if err := GenerateCerts(dir, s); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ca", "key.pem")); string(b) != string(caKey) {
		t.Error("the CA's key changed")
	}
	// Server certificates without the CA's key are only verified.
	os.Remove(filepath.Join(dir, "ca", "key.pem"))
	before, _ := os.ReadFile(filepath.Join(dir, "server", "cert.pem"))
	if err := GenerateCerts(dir, s); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "server", "cert.pem")); string(after) != string(before) {
		t.Error("a server certificate without the CA's key was replaced")
	}
}
