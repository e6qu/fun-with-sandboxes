module github.com/e6qu/fun-with-sandboxes

go 1.26.8

require (
	github.com/Microsoft/go-winio v0.6.3-0.20251027160822-ad3df93bed29
	github.com/ProtonMail/go-crypto v1.4.1
	github.com/agext/levenshtein v1.2.3
	github.com/armon/circbuf v0.0.0-20190214190532-5111143e8da2
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/containerd/accelerated-container-image v1.3.0
	github.com/containerd/containerd/api v1.12.0
	github.com/containerd/containerd/v2 v2.4.1
	github.com/containerd/continuity v0.5.0
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/log v0.2.0
	github.com/containerd/nydus-snapshotter v0.15.15
	github.com/containerd/platforms v1.0.0-rc.5
	github.com/containerd/stargz-snapshotter/estargz v0.18.2
	github.com/containerd/typeurl/v2 v2.3.0
	github.com/creack/pty v1.1.24
	github.com/cyphar/filepath-securejoin v0.7.0
	github.com/distribution/reference v0.6.0
	github.com/docker/go-units v0.5.0
	github.com/go-sql-driver/mysql v1.10.1
	github.com/gofrs/flock v0.13.1
	github.com/gohugoio/hashstructure v0.6.0
	github.com/google/go-containerregistry v0.22.1
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510
	github.com/hashicorp/go-immutable-radix/v2 v2.1.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/hiddeco/sshsig v0.2.0
	github.com/in-toto/in-toto-golang v0.11.0
	github.com/klauspost/compress v1.20.1
	github.com/moby/docker-image-spec v1.3.1
	github.com/moby/go-archive v0.2.0
	github.com/moby/locker v1.0.1
	github.com/moby/moby/api v1.56.1
	github.com/moby/patternmatcher v0.6.1
	github.com/moby/policy-helpers v0.0.0-20261006174519-bd98f4747414
	github.com/moby/sys/mountinfo v0.7.2
	github.com/moby/sys/reexec v0.1.0
	github.com/moby/sys/signal v0.7.1
	github.com/moby/sys/user v0.4.1
	github.com/moby/sys/userns v0.2.1
	github.com/moby/term v0.5.2
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
	github.com/opencontainers/runtime-spec v1.3.0
	github.com/package-url/packageurl-go v0.1.7
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/pkg/errors v0.9.1
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10
	github.com/prometheus/procfs v0.21.1
	github.com/sigstore/sigstore-go v1.2.2
	github.com/sirupsen/logrus v1.10.2
	github.com/spdx/tools-golang v0.5.7
	github.com/tonistiigi/dchapes-mode v0.0.0-20250318174251-73d941a28323
	github.com/tonistiigi/fsutil v0.0.0-20260819142231-83cac42c1c52
	github.com/tonistiigi/go-archvariant v1.0.0
	github.com/tonistiigi/go-csvvalue v0.0.0-20240814133006-030d3b2625d0
	github.com/tonistiigi/units v0.0.0-20180711220420-6950e57a87ea
	go.etcd.io/bbolt v1.5.0
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.71.0
	go.opentelemetry.io/contrib/instrumentation/net/http/httptrace/otelhttptrace v0.71.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0
	go.opentelemetry.io/otel/metric v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/sdk/metric v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
	go.opentelemetry.io/proto/otlp v1.11.0
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.57.0
	golang.org/x/mod v0.41.0
	golang.org/x/net v0.59.0
	golang.org/x/sync v0.23.0
	golang.org/x/sys v0.49.0
	golang.org/x/time v0.16.0
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260825221802-da73d73af1c5
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
	k8s.io/cri-api v0.37.2
	k8s.io/cri-streaming v0.37.2
	k8s.io/utils v0.0.0-20260626114624-be93311217bd
	tags.cncf.io/container-device-interface v1.1.1
)

require (
	cyphar.com/go-pathrs v0.2.5 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/Microsoft/hcsshim v0.15.0-rc.4 // indirect
	github.com/anchore/go-struct-converter v0.1.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/containerd/cgroups/v3 v3.1.3 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/fifo v1.1.0 // indirect
	github.com/containerd/log/otel v0.1.0 // indirect
	github.com/containerd/ttrpc v1.2.10 // indirect
	github.com/docker/cli v29.8.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.9 // indirect
	github.com/emicklei/go-restful/v3 v3.13.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/in-toto/attestation v1.2.0 // indirect
	github.com/moby/spdystream v0.5.1 // indirect
	github.com/moby/sys/mount v0.3.5 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/secure-systems-lab/go-securesystemslib v0.11.0 // indirect
	github.com/shibumi/go-pathspec v1.3.0 // indirect
	github.com/sigstore/sigstore v1.10.8 // indirect
	github.com/vbatts/tar-split v0.12.3 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	golang.org/x/term v0.46.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260825221802-da73d73af1c5 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/streaming v0.37.2 // indirect
	tags.cncf.io/container-device-interface/specs-go v1.1.1 // indirect
)
