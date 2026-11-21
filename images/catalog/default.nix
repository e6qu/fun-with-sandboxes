# The catalog's packages at their newest upstream releases. nixpkgs-26.05,
# whose packages the images otherwise take, lags upstream for these:
# nixpkgs-unstable's packages where it carries the newest release, and its
# recipes, copied here, at the newest release where it does not.
{ unstable, system ? builtins.currentSystem }:
let
  u = (builtins.getFlake unstable).legacyPackages.${system};
  kafka = u.callPackage (import ./kafka.nix {
    kafkaVersion = "4.4.0";
    scalaVersion = "2.13";
    hash = "sha256-XUbfFujb0ij+2Dw+b8QwaRbPPZFWGEp/wkYumtlxGn0=";
  }) { };
  gitea = u.callPackage ./gitea.nix { inherit gitea; };
  mailpit = u.callPackage ./mailpit.nix { inherit mailpit; };
  # The web UI's build Qdrant's release downloads into its image.
  qdrant-web-ui = u.fetchzip {
    url = "https://github.com/qdrant/qdrant-web-ui/releases/download/v0.2.21/dist-qdrant.zip";
    hash = "sha256-KL3phK5wAs471xdbJblxBvOTDzeeaMs03H4TDni1CJw=";
  };
  docker = u.docker_29.override {
    version = "29.10.0";
    cliRev = "v29.10.0";
    cliHash = "sha256-/S2az+6ki/jLk88JDtMI65MdhQIQpfiWMeF/BH/+9H8=";
    mobyRev = "docker-v29.10.0";
    mobyHash = "sha256-RddcicfQYA+ygWQJFseL+BvnEM3HkUiA3yaUs7IYz60=";
    clientOnly = true;
  };
  # ClickHouse's macOS build of the release, which its release publishes:
  # one binary, run as each of its tools by the name it is run under, as
  # nixpkgs' package lays it out.
  clickhouse = u.stdenvNoCC.mkDerivation {
    pname = "clickhouse";
    version = "26.10.8.34-stable";
    src = u.fetchurl {
      url = "https://github.com/ClickHouse/ClickHouse/releases/download/v26.10.8.34-stable/clickhouse-macos-aarch64";
      hash = "sha256-WkMHUK5fc35fQ4SAsFLEzsmtpOrmv7gtKCBkinQjT0M=";
    };
    dontUnpack = true;
    # The release's binary is signed as it is built; re-signing fails on it.
    dontFixup = true;
    installPhase = ''
      install -Dm755 $src $out/bin/clickhouse
      for t in ch chc chl benchmark check-marks checksum-for-compressed-block client compressor disks \
        docker-init extract-from-config format git-import keeper keeper-bench keeper-client keeper-converter \
        keeper-data-dumper keeper-utils local obfuscator server static-files-disk-uploader su \
        zookeeper-dump-tree zookeeper-remove-by-list; do
        case $t in ch|chc|chl) ln -s clickhouse $out/bin/$t ;; *) ln -s clickhouse $out/bin/clickhouse-$t ;; esac
      done
    '';
  };
in
{
  # nixpkgs-unstable's, the newest releases.
  inherit (u) redis nats-server neo4j mongodb-ce mongosh;
  nginx = u.nginxMainline;
  # Newer than nixpkgs-unstable's.
  apacheKafka = kafka;
  inherit gitea mailpit qdrant-web-ui clickhouse;
  bun = u.callPackage ./bun.nix { };
  meilisearch = u.callPackage ./meilisearch.nix { };
  podman = u.callPackage ./podman.nix { };
  qdrant = u.callPackage ./qdrant.nix { };
  rabbitmq-server = u.callPackage ./rabbitmq-server.nix { };
  seaweedfs = u.callPackage ./seaweedfs.nix { };
  toxiproxy = u.callPackage ./toxiproxy.nix { };
  valkey = u.callPackage ./valkey.nix { };
  vault-bin = u.callPackage ./vault-bin.nix { };
  influxdb2-server = u.callPackage ./influxdb2-server.nix { };
  docker-client = docker;
  docker-buildx = u.callPackage ./docker-buildx.nix { };
  docker-compose = u.callPackage ./docker-compose.nix { };
}
