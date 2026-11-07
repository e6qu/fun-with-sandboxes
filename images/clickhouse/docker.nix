# The files ClickHouse's server image adds to its packages, and the server
# configuration its packages install, from the source of the release
# nixpkgs builds.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  src = "https://raw.githubusercontent.com/ClickHouse/ClickHouse/v${pkgs.clickhouse.version}";
  file = path: hash: { name = baseNameOf path; path = pkgs.fetchurl { url = "${src}/${path}"; inherit hash; }; };
in
pkgs.linkFarm "clickhouse-docker" [
  (file "docker/server/entrypoint.sh" "sha256-v9ZC2Vh67EMqHLdhHs8VcJf1hWZyN00JamcSF6QzAp8=")
  (file "docker/server/docker_related_config.xml" "sha256-3JVpZuGb+A6ZamJ5UNCbHvWqUByodi9oyVxcQxvTPH0=")
  (file "programs/server/config.xml" "sha256-jkrZ662AWgsWjIT0qf4icqj/6xVydP/2wXmMsirLxqk=")
  (file "programs/server/users.xml" "sha256-I0A3KYMpKq+xyUz4aZWMxwehxCRR97k8JQd5mlflSuE=")
]
