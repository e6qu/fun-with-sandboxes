// Command dockerd serves, in a privileged fwsd container, the API of the
// engine fwsd nests in it, where dockerd would serve its own: on the
// sockets and TCP addresses its --host flags name, with dockerd's TLS
// flags. fwsd runs the nested engine's containers, each in a sandbox of
// its own, which no process in a sandboxed container could apply; this
// relays each connection to that engine, whose socket FWS_ENGINE names.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Options are the dockerd flags the nested engine honours.
type Options struct {
	Hosts             []string
	TLS, TLSVerify    *bool
	CACert, Cert, Key string
	Debug             bool
	PIDFile           string
}

// Parse reads dockerd's flags, in their -H, --host and --flag=value
// forms. Any other dockerd flag configures a daemon the nested engine is
// not, so it is refused rather than passed over.
func Parse(args []string) (Options, error) {
	o := Options{PIDFile: "/var/run/docker.pid"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		next := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag needs an argument: %s", name)
			}
			i++
			return args[i], nil
		}
		boolean := func() (*bool, error) {
			if !hasValue {
				t := true
				return &t, nil
			}
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q for %s", value, name)
			}
			return &b, nil
		}
		var err error
		switch name {
		case "-H", "--host":
			var h string
			if h, err = next(); err == nil {
				o.Hosts = append(o.Hosts, h)
			}
		case "--tls":
			o.TLS, err = boolean()
		case "--tlsverify":
			o.TLSVerify, err = boolean()
		case "--tlscacert":
			o.CACert, err = next()
		case "--tlscert":
			o.Cert, err = next()
		case "--tlskey":
			o.Key, err = next()
		case "-D", "--debug":
			var b *bool
			if b, err = boolean(); err == nil {
				o.Debug = *b
			}
		case "-p", "--pidfile":
			o.PIDFile, err = next()
		default:
			return o, fmt.Errorf("%s is not supported: this dockerd serves the engine fwsd nests in the container, which takes no daemon flags but its listeners' and TLS's", name)
		}
		if err != nil {
			return o, err
		}
	}
	if len(o.Hosts) == 0 {
		o.Hosts = []string{"unix:///var/run/docker.sock"}
	}
	return o, nil
}

// Secure reports whether the TCP listeners use TLS, and whether they
// verify clients, as dockerd decides from its flags.
func (o Options) Secure() (useTLS, verify bool) {
	verify = o.TLSVerify != nil && *o.TLSVerify
	useTLS = verify || (o.TLS != nil && *o.TLS)
	return useTLS, verify
}

func main() {
	log.SetFlags(0)
	o, err := Parse(os.Args[1:])
	if err != nil {
		log.Fatalf("dockerd: %v", err)
	}
	engine := os.Getenv("FWS_ENGINE")
	if engine == "" {
		log.Fatal("dockerd: no engine to serve: run the container with --privileged, which gives it an engine of its own")
	}
	var tlsConfig *tls.Config
	useTLS, verify := o.Secure()
	if useTLS {
		cert, err := tls.LoadX509KeyPair(o.Cert, o.Key)
		if err != nil {
			log.Fatalf("dockerd: could not load the TLS certificate: %v", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if verify {
			pem, err := os.ReadFile(o.CACert)
			if err != nil {
				log.Fatalf("dockerd: could not read the CA certificate: %v", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				log.Fatalf("dockerd: no certificate in %s", o.CACert)
			}
			tlsConfig.ClientCAs, tlsConfig.ClientAuth = pool, tls.RequireAndVerifyClientCert
		}
	}
	var listeners []net.Listener
	for _, h := range o.Hosts {
		l, err := listen(h, tlsConfig)
		if err != nil {
			log.Fatalf("dockerd: %v", err)
		}
		listeners = append(listeners, l)
		if l.Addr().Network() == "tcp" && !useTLS {
			log.Printf("WARNING: API is accessible on http://%s without encryption.", l.Addr())
		}
		log.Printf("API listen on %s", l.Addr())
	}
	if err := os.WriteFile(o.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		log.Fatalf("dockerd: %v", err)
	}
	defer os.Remove(o.PIDFile)

	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			relay(l, engine, o.Debug)
		}(l)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	s := <-sigs
	log.Printf("Processing signal '%v'", s)
	for _, l := range listeners {
		l.Close()
	}
	wg.Wait()
	log.Print("Daemon shutdown complete")
}

// listen opens one of dockerd's --host addresses: unix://path, or
// tcp://host:port with TLS when the flags ask for it.
func listen(host string, tlsConfig *tls.Config) (net.Listener, error) {
	proto, addr, ok := strings.Cut(host, "://")
	if !ok {
		return nil, fmt.Errorf("invalid bind address format: %s", host)
	}
	switch proto {
	case "unix":
		os.Remove(addr)
		l, err := net.Listen("unix", addr)
		if err != nil {
			return nil, err
		}
		return l, os.Chmod(addr, 0o660)
	case "tcp":
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("invalid bind address format: %s", host)
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		if tlsConfig != nil {
			l = tls.NewListener(l, tlsConfig)
		}
		return l, nil
	}
	return nil, fmt.Errorf("%s listeners are not supported: only unix and tcp ones are", proto)
}

// relay passes each connection l accepts to the engine's socket, both
// ways, closing each side's writes as the other ends them, so hijacked
// attach and exec streams end as they would on dockerd.
func relay(l net.Listener, engine string, debug bool) {
	for {
		c, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept: %v", err)
			}
			return
		}
		go func() {
			defer c.Close()
			up, err := net.Dial("unix", engine)
			if err != nil {
				log.Printf("the engine: %v", err)
				return
			}
			defer up.Close()
			if debug {
				log.Printf("relaying %s", c.RemoteAddr())
			}
			done := make(chan struct{})
			go func() {
				io.Copy(up, c)
				closeWrite(up)
				close(done)
			}()
			io.Copy(c, up)
			closeWrite(c)
			<-done
		}()
	}
}

func closeWrite(c net.Conn) {
	switch x := c.(type) {
	case *net.UnixConn:
		x.CloseWrite()
	case *net.TCPConn:
		x.CloseWrite()
	case *tls.Conn:
		x.CloseWrite()
	}
}
