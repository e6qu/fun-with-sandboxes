// Package catalog lists the darwin images fws builds and publishes, as
// scripts/build-images.sh builds them and docs/IMAGES.md documents them.
package catalog

// Registry is where CI publishes the catalog: fws/<name>:<tag> as
// <Registry>/fws-<name>:<tag>.
const Registry = "ghcr.io/e6qu"

// Kind is what an image of the catalog is for, by which docs/IMAGES.md and
// `fws catalog` group them.
type Kind string

const (
	Base       Kind = "base"
	Language   Kind = "language"
	Lambda     Kind = "lambda"
	Tool       Kind = "tool"
	Database   Kind = "database"
	Storage    Kind = "storage"
	Messaging  Kind = "messaging"
	Service    Kind = "service"
	Kubernetes Kind = "kubernetes"
)

// Image is one of the catalog's images and the tags it is built at.
type Image struct {
	Name    string   `json:"name"`
	Tags    []string `json:"tags"`
	Kind    Kind     `json:"kind"`
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
	{"base", []string{"1"}, Base, "bash, BusyBox-style commands, curl, git, jq and a CA bundle, to build FROM"},
	{"busybox", []string{"1"}, Base, "a shell and the everyday commands scripts run"},
	{"python", []string{"3.11", "3.12", "3.13", "3.14", "3.15"}, Language, "Python with pip"},
	{"node", []string{"22", "24", "26"}, Language, "Node.js with npm, and clang, make and python3 for native addons"},
	{"bun", []string{"1"}, Language, "Bun"},
	{"rust", []string{"1.100"}, Language, "Rust with cargo, rustfmt, clippy, and clang to link and build C"},
	{"lambda/python", []string{"3.11", "3.12", "3.13", "3.14"}, Lambda, "an AWS Lambda Python runtime under the Runtime Interface Emulator"},
	{"lambda/nodejs", []string{"22", "24"}, Lambda, "an AWS Lambda Node.js runtime under the Runtime Interface Emulator"},
	{"lambda/java", []string{"17"}, Lambda, "an AWS Lambda Java runtime with a JDK"},
	{"playwright", []string{"1.65"}, Tool, "Node.js 24 with Playwright, its test runner, Chromium and Firefox"},
	{"postgres", []string{"16", "18"}, Database, "PostgreSQL"},
	{"mysql", []string{"8.4"}, Database, "MySQL"},
	{"mariadb", []string{"11"}, Database, "MariaDB"},
	{"mongo", []string{"8.3"}, Database, "MongoDB with mongosh"},
	{"redis", []string{"8"}, Database, "Redis"},
	{"valkey", []string{"9"}, Database, "Valkey"},
	{"memcached", []string{"1.6"}, Database, "Memcached"},
	{"etcd", []string{"3.7"}, Database, "etcd with etcdctl and etcdutl"},
	{"neo4j", []string{"2026.10"}, Database, "Neo4j Community Edition"},
	{"clickhouse", []string{"26.10"}, Database, "ClickHouse"},
	{"influxdb", []string{"2.10"}, Database, "InfluxDB 2 with the influx CLI"},
	{"opensearch", []string{"3.10"}, Database, "OpenSearch with its security plugin"},
	{"qdrant", []string{"1.20"}, Database, "Qdrant with its web UI"},
	{"meilisearch", []string{"1.57"}, Database, "Meilisearch"},
	{"dynamodb-local", []string{"2"}, Database, "DynamoDB Local"},
	{"seaweedfs", []string{"4.54"}, Storage, "SeaweedFS: S3, filer, WebDAV"},
	{"kafka", []string{"4.4"}, Messaging, "Apache Kafka in Confluent's layout, KRaft"},
	{"rabbitmq", []string{"4"}, Messaging, "RabbitMQ"},
	{"nats", []string{"2.15"}, Messaging, "NATS"},
	{"mailpit", []string{"1.34"}, Service, "Mailpit: SMTP, POP3 and a web UI"},
	{"consul", []string{"2.1"}, Service, "Consul"},
	{"vault", []string{"2.2"}, Service, "Vault"},
	{"gitea", []string{"28"}, Service, "Gitea"},
	{"nginx", []string{"1"}, Service, "nginx with the official image's templates"},
	{"toxiproxy", []string{"2.13"}, Tool, "Toxiproxy with its CLI"},
	{"dind", []string{"29"}, Tool, "Docker in Docker: an engine of its own, with the Docker CLI, buildx and Compose"},
	{"ryuk", []string{"0.14.0"}, Tool, "Testcontainers' reaper"},
	{"toolbox", []string{"1"}, Tool, "the test suite's app and probe"},
	{"kind-node", []string{"1.37"}, Kubernetes, "kind's node: kubeadm, the kubelet and fws-kube-proxy"},
	{"k3s", []string{"1.37"}, Kubernetes, "k3s's image, a single-node kubeadm cluster"},
	{"kube-apiserver", []string{"1.37"}, Kubernetes, "Kubernetes' API server"},
	{"kube-controller-manager", []string{"1.37"}, Kubernetes, "Kubernetes' controller manager"},
	{"kube-scheduler", []string{"1.37"}, Kubernetes, "Kubernetes' scheduler"},
	{"kubectl", []string{"1.37"}, Kubernetes, "kubectl"},
	{"kubelet", []string{"1.37"}, Kubernetes, "the kubelet with its darwin parts"},
	{"pause", []string{"3.10.2"}, Kubernetes, "Kubernetes' pause program"},
}
