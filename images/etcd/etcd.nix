# etcd at its newest release, which nixpkgs does not package yet: its
# etcd_3_6 package, the server, etcdutl and etcdctl each built from their
# module of the release's source, given 3.7's source and vendored modules.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  version = "3.7.4";
  src = pkgs.fetchFromGitHub {
    owner = "etcd-io";
    repo = "etcd";
    tag = "v${version}";
    hash = "sha256-cml5WxaPa9zh1h26hD0o48rKIWdf/TuUKPCT0SboEGc=";
  };
  vendorHashes = {
    etcdserver = "sha256-A1oIxpTw0LbJ+0nIat+oPnet0opSUBeL39DtemV61qk=";
    etcdutl = "sha256-qWZCF/NT/lG/y3WjP9joOx2hmveFXeWu/AC2RN6yrDw=";
    etcdctl = "sha256-JbmYUwAoP6xmNsKC8fwLTB+mobEBE0eVaNprnnLx7NM=";
  };
  # 3.7's source is a Go workspace, which go mod vendor and the build each
  # leave for the module they build: the vendored modules' derivation takes
  # the build's environment.
  deps = builtins.mapAttrs (name: vendorHash:
    pkgs.etcd_3_6.passthru.deps.${name}.overrideAttrs (old: {
      inherit version src vendorHash;
      env = old.env // { GOWORK = "off"; };
    })) vendorHashes;
in
pkgs.symlinkJoin {
  pname = "etcd";
  inherit version;
  paths = builtins.attrValues deps;
  passthru = { inherit deps; };
}
