//go:build e2e && darwin

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	tcmailpit "github.com/testcontainers/testcontainers-go/modules/mailpit"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Kafka runs in KRaft mode as Testcontainers' module drives Confluent's
// images: the module's script edits and runs the image's configure and
// launch scripts, and a client on the host produces and consumes through
// the listener the broker advertises on its host port.
func TestKafka(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	k, err := tckafka.Run(ctx, "fws/kafka:4.2", tckafka.WithClusterID("kraftCluster"))
	testcontainers.CleanupContainer(t, k)
	if err != nil {
		t.Fatal(err)
	}
	if k.ClusterID != "kraftCluster" {
		t.Errorf("the module read cluster ID %q", k.ClusterID)
	}
	brokers, err := k.Brokers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.AllowAutoTopicCreation())
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	if err := producer.ProduceSync(ctx, &kgo.Record{Topic: "notes", Value: []byte("kept")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	consumer, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics("notes"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	fetches := consumer.PollFetches(ctx)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	if rs := fetches.Records(); len(rs) != 1 || string(rs[0].Value) != "kept" {
		t.Fatalf("consumed %v", rs)
	}
}

// Without the module, the image configures, formats and launches a broker
// from its environment alone, as Confluent's images do, logging at the
// levels it names, and Kafka's own tools in another container reach it by
// its alias on the network they share.
func TestKafkaOnANetwork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	nw, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Remove(context.Background())
	broker, err := testcontainers.Run(ctx, "fws/kafka:4.2",
		testcontainers.WithEnv(map[string]string{
			"KAFKA_ADVERTISED_LISTENERS": "PLAINTEXT://kafka:9092",
			"KAFKA_LOG4J_ROOT_LOGLEVEL":  "WARN",
			"KAFKA_LOG4J_LOGGERS":        "kafka.server.BrokerLifecycleManager=INFO",
		}),
		testcontainers.WithWaitStrategy(wait.ForLog("Transitioning from RECOVERY to RUNNING").WithStartupTimeout(3*time.Minute)),
		tcnetwork.WithNetwork([]string{"kafka"}, nw))
	testcontainers.CleanupContainer(t, broker)
	if err != nil {
		t.Fatal(err)
	}
	const script = `set -e
kafka-topics --bootstrap-server kafka:9092 --create --topic notes --partitions 1 --replication-factor 1
echo kept | kafka-console-producer --bootstrap-server kafka:9092 --topic notes
kafka-console-consumer --bootstrap-server kafka:9092 --topic notes --from-beginning --max-messages 1 --timeout-ms 60000 | sed 's/^/consumed /'`
	client, err := testcontainers.Run(ctx, "fws/kafka:4.2",
		testcontainers.WithCmd("sh", "-c", script),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(3*time.Minute)),
		tcnetwork.WithNetwork(nil, nw))
	testcontainers.CleanupContainer(t, client)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := client.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(logs)
	if state.ExitCode != 0 || !strings.Contains(string(b), "consumed kept") {
		t.Fatalf("the client exited %d:\n%s", state.ExitCode, b)
	}
	brokerLogs, err := broker.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(brokerLogs)
	if strings.Contains(string(b), "Kafka Server started") || strings.Contains(string(b), "StatusLogger") {
		t.Errorf("the broker logged below its root level, or about its logging:\n%s", b)
	}
	r, err := broker.CopyFileFromContainer(ctx, "/etc/kafka/kafka.properties")
	if err != nil {
		t.Fatal(err)
	}
	props, _ := io.ReadAll(r)
	r.Close()
	for _, want := range []string{"advertised.listeners=PLAINTEXT://kafka:9092\n", "log.dirs=/var/lib/kafka/data\n", "process.roles=broker,controller\n"} {
		if !strings.Contains(string(props), want) {
			t.Errorf("kafka.properties lacks %q:\n%s", want, props)
		}
	}
}

// Mailpit takes mail on its SMTP port, with the credentials the module
// sets and no others, and lists it through its REST API.
func TestMailpit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	m, err := tcmailpit.Run(ctx, "fws/mailpit:1.32", tcmailpit.WithSMTPAuth("sender", "secret"))
	testcontainers.CleanupContainer(t, m)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := m.SMTPEndpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: kept\r\n\r\nbody\r\n")
	if err := smtp.SendMail(endpoint, smtp.PlainAuth("", "sender", "wrong", host), "a@example.com", []string{"b@example.com"}, msg); err == nil {
		t.Error("a wrong password sent mail")
	}
	if err := smtp.SendMail(endpoint, smtp.PlainAuth("", "sender", "secret", host), "a@example.com", []string{"b@example.com"}, msg); err != nil {
		t.Fatal(err)
	}
	base, err := m.HTTPURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code, body := httpDo(t, "GET", base+"/api/v1/messages", "", nil)
	var list struct {
		Total    int
		Messages []struct{ Subject string }
	}
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || list.Total != 1 || len(list.Messages) != 1 || list.Messages[0].Subject != "kept" {
		t.Fatalf("the messages: %d %s", code, body)
	}
}

// Mailpit's image checks its health with mailpit readyz, every second
// until it first passes.
func TestMailpitHealth(t *testing.T) {
	const name = "fws-e2e-mailpit"
	waitedRun(t, name, "fws/mailpit:1.32")
	if out := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.Config.Healthcheck.StartInterval}}", name)); out != "1s" {
		t.Errorf("the health check's start interval: %q", out)
	}
}
