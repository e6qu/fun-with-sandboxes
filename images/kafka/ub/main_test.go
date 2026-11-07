package main

import "testing"

func TestPropertyName(t *testing.T) {
	for in, want := range map[string]string{
		"ADVERTISED_LISTENERS":           "advertised.listeners",
		"LOG4J_LOGGER_KAFKA__AUTHORIZER": "log4j.logger.kafka_authorizer",
		"SSL_KEY___PASSWORD":             "ssl.key-password",
		"CONFLUENT_SUPPORT_METRICS":      "confluent.support.metrics",
	} {
		if got := PropertyName(in); got != want {
			t.Errorf("PropertyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	spec := Spec{
		Prefixes: map[string]bool{"KAFKA": false, "CONFLUENT": true},
		Excludes: []string{"KAFKA_HEAP_OPTS"},
		Renamed:  map[string]string{"KAFKA_ZOOKEEPER_CLIENT_CNXN_SOCKET": "zookeeper.clientCnxnSocket"},
		Defaults: map[string]string{"log.dirs": "/var/lib/kafka/data", "node.id": "0"},
	}
	got := Render(spec, []string{
		"KAFKA_NODE_ID=1",
		"KAFKA_LISTENERS=PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093",
		"KAFKA_HEAP_OPTS=-Xmx1G",
		"KAFKA_ZOOKEEPER_CLIENT_CNXN_SOCKET=org.apache.zookeeper.ClientCnxnSocketNetty",
		"CONFLUENT_METRICS_ENABLE=false",
		"KAFKAESQUE=no",
		"KAFKA_=empty",
		"PATH=/bin",
		"KAFKA_SASL_JAAS_CONFIG=a=b",
	})
	want := "confluent.metrics.enable=false\n" +
		"listeners=PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093\n" +
		"log.dirs=/var/lib/kafka/data\n" +
		"node.id=1\n" +
		"sasl.jaas.config=a=b\n" +
		"zookeeper.clientCnxnSocket=org.apache.zookeeper.ClientCnxnSocketNetty\n"
	if got != want {
		t.Errorf("Render:\n%s\nwant:\n%s", got, want)
	}
}
