# The pause program a pod sandbox's infra container runs, from Kubernetes'
# source at the release kind's node image ships, built as its build/pause
# Makefile builds it, linked against libSystem as macOS programs are.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  kubernetes = import ./kubernetes.nix { inherit nixpkgs system; };
  version = "3.10.1";
in
pkgs.stdenv.mkDerivation {
  pname = "pause";
  inherit version;
  inherit (kubernetes) src;
  dontConfigure = true;
  buildPhase = ''
    $CC -Os -Wall -Werror -DVERSION=v${version} -o pause build/pause/linux/pause.c
  '';
  installPhase = ''
    install -Dm755 pause $out/bin/pause
  '';
}
