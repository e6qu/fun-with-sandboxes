// Command entry runs RabbitMQ inside a fwsd container. Every port it opens
// goes on loopback, on the ports the engine assigned: AMQP on 5672, the
// Erlang port mapper on 4369 and Erlang distribution on 25672, which RabbitMQ
// otherwise takes on every interface and at fixed numbers that stacks would
// share. Its data and cookie live under /var/lib/rabbitmq.
package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("entry: ")
	amqp, err := sandboxenv.Port("5672")
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := handshake("127.0.0.1:" + amqp); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	epmd, err := sandboxenv.Port("4369")
	if err != nil {
		log.Fatal(err)
	}
	dist, err := sandboxenv.Port("25672")
	if err != nil {
		log.Fatal(err)
	}
	home := sandboxenv.Path("/var/lib/rabbitmq")
	etc := sandboxenv.Path("/etc/rabbitmq")
	for _, d := range []string{home, etc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	plugins := filepath.Join(etc, "enabled_plugins")
	if _, err := os.Stat(plugins); os.IsNotExist(err) {
		if err := os.WriteFile(plugins, []byte("[].\n"), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	env := append(os.Environ(),
		"HOME="+home,
		"RABBITMQ_NODENAME=rabbit@localhost",
		"RABBITMQ_NODE_IP_ADDRESS=127.0.0.1",
		"RABBITMQ_NODE_PORT="+amqp,
		"RABBITMQ_DIST_PORT="+dist,
		"ERL_EPMD_PORT="+epmd,
		"ERL_EPMD_ADDRESS=127.0.0.1",
		"RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS=-kernel inet_dist_use_interface {127,0,0,1}",
		"RABBITMQ_MNESIA_BASE="+filepath.Join(home, "mnesia"),
		"RABBITMQ_LOG_BASE="+filepath.Join(home, "log"),
		"RABBITMQ_LOGS=-",
		"RABBITMQ_CONFIG_FILE="+filepath.Join(etc, "rabbitmq"),
		"RABBITMQ_ADVANCED_CONFIG_FILE="+filepath.Join(etc, "advanced.config"),
		"RABBITMQ_ENABLED_PLUGINS_FILE="+plugins,
	)
	// The script finds its siblings beside the path it runs from.
	server, err := filepath.EvalSymlinks(sandboxenv.Path("/bin/rabbitmq-server"))
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] != "rabbitmq-server" {
		log.Fatalf("unknown command %q: the image runs rabbitmq-server", os.Args[1])
	}
	log.Fatal(syscall.Exec(server, []string{"rabbitmq-server"}, env))
}

// handshake opens an AMQP 0-9-1 connection far enough for the broker to
// send Connection.Start, which it does once it serves.
func handshake(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("AMQP\x00\x00\x09\x01")); err != nil {
		return err
	}
	head := make([]byte, 7)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	// A method frame (type 1) on channel 0.
	if head[0] != 1 || !bytes.Equal(head[1:3], []byte{0, 0}) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
