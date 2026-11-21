# The kubelet for macOS: Kubernetes' kubelet at the release of the control
# plane, with the darwin parts upstream's tree lacks (kubelet-darwin.patch):
# the node's machine information and CPU, memory and filesystem use from
# its kernel, in cAdvisor's shapes, a container manager for a node without
# cgroups, the static pod manifests' watcher, its volumes' host utilities,
# mounts and subpaths without the mounts macOS allows no unprivileged
# process, and a container's memory and CPU limits sent to the runtime as
# a Linux container's resources.
{ nixpkgs, system ? builtins.currentSystem }:
(import ./kubernetes.nix { inherit nixpkgs system; }).overrideAttrs (old: {
  pname = "kubelet";
  patches = [ ./kubelet-darwin.patch ];
  env = old.env // { WHAT = "cmd/kubelet"; };
})
