// Package catalog lists the darwin images fws builds and publishes, as
// scripts/build-images.sh builds them and docs/IMAGES.md documents them.
package catalog

// Registry is where CI publishes the catalog: fws/<name>:<tag> as
// <Registry>/fws-<name>:<tag>.
const Registry = "ghcr.io/e6qu"

// Image is one of the catalog's images and the tags it is built at.
type Image struct {
	Name    string   `json:"name"`
	Tags    []string `json:"tags"`
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
}

// Ref is the image's name at tag, as docker run takes it: fws/<name>:<tag>.
func (i Image) Ref(tag string) string { return "fws/" + i.Name + ":" + tag }

// Published is where the image is published at tag.
func (i Image) Published(tag string) string {
	return Registry + "/fws-" + i.Name + ":" + tag
}

// Images is the catalog, by kind and name.
var Images = []Image{
	{"base", []string{"1"}, "base", "bash, BusyBox-style commands, curl, git, jq and a CA bundle, to build FROM"},
	{"busybox", []string{"1"}, "base", "a shell and the everyday commands scripts run"},
	{"python", []string{"3.11", "3.12", "3.13", "3.14", "3.15"}, "language", "Python with pip"},
	{"node", []string{"22", "24", "26"}, "language", "Node.js with npm"},
	{"bun", []string{"1"}, "language", "Bun"},
	{"lambda/python", []string{"3.11", "3.12", "3.13", "3.14"}, "lambda", "an AWS Lambda Python runtime under the Runtime Interface Emulator"},
	{"lambda/nodejs", []string{"22", "24"}, "lambda", "an AWS Lambda Node.js runtime under the Runtime Interface Emulator"},
	{"lambda/java", []string{"17"}, "lambda", "an AWS Lambda Java runtime with a JDK"},
	{"playwright", []string{"1.65"}, "tool", "Node.js 24 with Playwright and Chromium's headless shell"},
	{"postgres", []string{"16", "18"}, "database", "PostgreSQL"},
	{"mysql", []string{"8.4"}, "database", "MySQL"},
	{"mariadb", []string{"11"}, "database", "MariaDB"},
	{"mongo", []string{"8.3"}, "database", "MongoDB with mongosh"},
	{"redis", []string{"8"}, "database", "Redis"},
	{"valkey", []string{"9"}, "database", "Valkey"},
	{"memcached", []string{"1.6"}, "database", "Memcached"},
	{"etcd", []string{"3.7"}, "database", "etcd with etcdctl and etcdutl"},
	{"neo4j", []string{"2026.10"}, "database", "Neo4j Community Edition"},
	{"clickhouse", []string{"26.10"}, "database", "ClickHouse"},
	{"influxdb", []string{"2.10"}, "database", "InfluxDB 2 with the influx CLI"},
	{"opensearch", []string{"3.10"}, "database", "OpenSearch with its security plugin"},
	{"qdrant", []string{"1.20"}, "database", "Qdrant with its web UI"},
	{"meilisearch", []string{"1.57"}, "database", "Meilisearch"},
	{"dynamodb-local", []string{"2"}, "database", "DynamoDB Local"},
	{"seaweedfs", []string{"4.54"}, "storage", "SeaweedFS: S3, filer, WebDAV"},
	{"kafka", []string{"4.4"}, "messaging", "Apache Kafka in Confluent's layout, KRaft"},
	{"rabbitmq", []string{"4"}, "messaging", "RabbitMQ"},
	{"nats", []string{"2.15"}, "messaging", "NATS"},
	{"mailpit", []string{"1.34"}, "service", "Mailpit: SMTP, POP3 and a web UI"},
	{"consul", []string{"2.1"}, "service", "Consul"},
	{"vault", []string{"2.2"}, "service", "Vault"},
	{"gitea", []string{"28"}, "service", "Gitea"},
	{"nginx", []string{"1"}, "service", "nginx with the official image's templates"},
	{"toxiproxy", []string{"2.13"}, "tool", "Toxiproxy with its CLI"},
	{"dind", []string{"29"}, "tool", "Docker in Docker: an engine of its own, with the Docker CLI, buildx and Compose"},
	{"ryuk", []string{"0.14.0"}, "tool", "Testcontainers' reaper"},
	{"toolbox", []string{"1"}, "tool", "the test suite's app and probe"},
	{"kind-node", []string{"1.37"}, "kubernetes", "kind's node: kubeadm, the kubelet and fws-kube-proxy"},
	{"k3s", []string{"1.37"}, "kubernetes", "k3s's image, a single-node kubeadm cluster"},
	{"kube-apiserver", []string{"1.37"}, "kubernetes", "Kubernetes' API server"},
	{"kube-controller-manager", []string{"1.37"}, "kubernetes", "Kubernetes' controller manager"},
	{"kube-scheduler", []string{"1.37"}, "kubernetes", "Kubernetes' scheduler"},
	{"kubectl", []string{"1.37"}, "kubernetes", "kubectl"},
	{"kubelet", []string{"1.37"}, "kubernetes", "the kubelet with its darwin parts"},
	{"pause", []string{"3.10.2"}, "kubernetes", "Kubernetes' pause program"},
}
