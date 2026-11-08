# go-runner, the entrypoint of Kubernetes' server images, from
# kubernetes/release at its v2.4.0 release, built as its Dockerfile builds it.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  rev = "7808a30ad8a91e3408d8b9a1cd214051d50d5538";
  file = name: hash: pkgs.fetchurl {
    url = "https://raw.githubusercontent.com/kubernetes/release/${rev}/images/build/go-runner/${name}";
    inherit hash;
  };
in
pkgs.buildGoModule {
  pname = "go-runner";
  version = "2.4.0";
  src = pkgs.runCommand "go-runner-src" { } ''
    mkdir $out
    cp ${file "go-runner.go" "sha256-oZp8HX+nVkv+/2kVsC2aJZMsvlJn61ntI4bW+neGq5Q="} $out/go-runner.go
    cp ${file "go.mod" "sha256-mX23LtFbjTus0FLwIaii+S2jiRPwOpwk2ozjWerA75E="} $out/go.mod
  '';
  vendorHash = null;
  env.CGO_ENABLED = 0;
  ldflags = [ "-s" "-w" ];
}
