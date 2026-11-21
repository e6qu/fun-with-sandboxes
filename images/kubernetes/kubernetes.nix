# Kubernetes' control plane and clients for macOS, at the release kind's
# node image ships, built with upstream's make as nixpkgs builds its Linux
# package, which declares Linux alone.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  version = "1.37.2";
in
pkgs.buildGoModule {
  pname = "kubernetes";
  inherit version;
  src = pkgs.fetchFromGitHub {
    owner = "kubernetes";
    repo = "kubernetes";
    tag = "v${version}";
    # The archive substitutes the refs that name the commit, which change.
    forceFetchGit = true;
    hash = "sha256-f8I5fKBYlPsMxtoI/NUdjGOsZ7n1nOdM06hUz0tA/gI=";
  };
  vendorHash = null;
  doCheck = false;
  nativeBuildInputs = [ pkgs.which pkgs.rsync ];
  env = {
    WHAT = "cmd/kube-apiserver cmd/kube-controller-manager cmd/kube-scheduler cmd/kubeadm cmd/kubectl";
    # What upstream's make reads from git, which the source archive lacks.
    KUBE_GIT_VERSION = "v${version}";
    KUBE_GIT_COMMIT = "c50e5395f1ae2d4a2c05f28fdcc38c0916cc1a32";
    KUBE_GIT_TREE_STATE = "clean";
    # Upstream links darwin's kubectl with cgo, against a libresolv the
    # images do not carry; kind's node binaries are static.
    CGO_ENABLED = "0";
  };
  buildPhase = ''
    runHook preBuild
    patchShebangs ./hack
    make "SHELL=${pkgs.runtimeShell}" "WHAT=$WHAT"
    runHook postBuild
  '';
  installPhase = ''
    runHook preInstall
    for p in $WHAT; do
      install -D _output/local/go/bin/''${p##*/} -t $out/bin
    done
    runHook postInstall
  '';
}
