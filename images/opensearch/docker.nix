# OpenSearch's minimal distribution of the version nixpkgs packages, and
# the files its image adds to it, from the opensearch-build release.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  src = "https://raw.githubusercontent.com/opensearch-project/opensearch-build/${pkgs.opensearch.version}";
  file = path: hash: { name = baseNameOf path; path = pkgs.fetchurl { url = "${src}/${path}"; inherit hash; }; };
in
pkgs.linkFarm "opensearch-docker" [
  {
    name = "opensearch-min.tar.gz";
    path = pkgs.fetchurl {
      url = "https://artifacts.opensearch.org/releases/core/opensearch/${pkgs.opensearch.version}/opensearch-min-${pkgs.opensearch.version}-linux-x64.tar.gz";
      hash = "sha256-9Q0pJx+9D1pVTvHRW9Bi78DwajH1c0bjOVbO1qWNnYI=";
    };
  }
  (file "docker/release/config/opensearch/opensearch-docker-entrypoint-3.x.sh" "sha256-0lQUw+aBg1uA13tq23rESe2v0DzvhR934CUuhutU9d0=")
  (file "docker/release/config/opensearch/log4j2.properties" "sha256-vq0bBOYatcKGk4XZWm7EZ07naN1+VxoDCSUam+uOq2s=")
  (file "docker/release/config/opensearch/performance-analyzer.properties" "sha256-v4o9FEdsvakYIduCmrX+qrZ+YWde7XrvemrI09XWGFs=")
  (file "config/opensearch.yml" "sha256-T21zOQ0EQ6tUmOKhOU19Z1s1i2Nnk9UniHtAOBppAU8=")
  (file "scripts/opensearch-onetime-setup.sh" "sha256-bCrv48p38f8rQ4QUKaTkJ9KlRnadComcVpXMSV0M3dc=")
]
