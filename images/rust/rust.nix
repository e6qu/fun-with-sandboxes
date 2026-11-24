# Rust at its newest stable release, which nixpkgs lags behind: the
# release's own macOS build, the components rustup's minimal profile
# installs in the official image (rustc, cargo and the standard library),
# with rustfmt and clippy, each checked against the digest the release's
# channel manifest (dist/channel-rust-<version>.toml) gives it.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  version = "1.100.0";
  date = "2026-11-12";
  target = "aarch64-apple-darwin";
  component = name: hash: pkgs.fetchurl {
    url = "https://static.rust-lang.org/dist/${date}/${name}-${version}-${target}.tar.xz";
    inherit hash;
  };
in
pkgs.stdenvNoCC.mkDerivation {
  pname = "rust";
  inherit version;
  srcs = [
    (component "rustc" "sha256-P81/Fwn2LdpYv02y3P1jCkUedQqtqqVRohzEpieje0w=")
    (component "cargo" "sha256-fhgmXS0s2o3sbKxND6o7uv6Se0AeUYU1Crx9Zp+a2vs=")
    (component "rust-std" "sha256-k0VkW29W7qKL+MwsD2KKYdUHpoj5HBvsWua/rSFqFsc=")
    (component "rustfmt" "sha256-3V9gxrh8n65L+nBtOyDOZI+eXFlUc6qaTPsxzsenqPo=")
    (component "clippy" "sha256-+1f2ObW5vPEmLfH/gbz8nkQzj7xymw1arVMblkk9Y5E=")
  ];
  sourceRoot = ".";
  dontConfigure = true;
  dontBuild = true;
  # Each component's installer, as rustup runs it, into one prefix.
  installPhase = ''
    runHook preInstall
    for d in */; do
      ${pkgs.bash}/bin/bash "$d/install.sh" --prefix="$out" --disable-ldconfig
    done
    runHook postInstall
  '';
  # The release's binaries load only macOS's libraries and their own, by
  # @rpath, and are signed as built; strip would also empty the rlibs'
  # metadata sections, as nixpkgs' rustc-bin notes.
  dontFixup = true;
}
