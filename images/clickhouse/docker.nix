# The files ClickHouse's server image adds to its packages, and the server
# configuration its packages install, from the source of the release the
# catalog's ClickHouse is (images/catalog).
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  src = "https://raw.githubusercontent.com/ClickHouse/ClickHouse/v26.10.8.34-stable";
  file = path: hash: { name = baseNameOf path; path = pkgs.fetchurl { url = "${src}/${path}"; inherit hash; }; };
in
pkgs.linkFarm "clickhouse-docker" [
  (file "docker/server/entrypoint.sh" "sha256-UOWAyofxX3ImZyXFq8zygGeGhuP6FDNxPz5FHIcG1x8=")
  (file "docker/server/docker_related_config.xml" "sha256-3JVpZuGb+A6ZamJ5UNCbHvWqUByodi9oyVxcQxvTPH0=")
  (file "programs/server/config.xml" "sha256-/6QezVDmM+fJi6VKUrBfswO5KiTxAmDzZnc7LDFTYZQ=")
  (file "programs/server/users.xml" "sha256-39wimcXsbl+nbGBdJGim3LoWb9n2X9TNcXgTTalZy18=")
]
