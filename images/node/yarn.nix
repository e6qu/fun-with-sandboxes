# Yarn 1, which the official node:22 and node:24 images carry beside
# corepack, without a Node.js of its own: its #!/usr/bin/env node runs the
# image's.
{ nixpkgs, system ? builtins.currentSystem }:
(builtins.getFlake nixpkgs).legacyPackages.${system}.yarn.override { withNode = false; }
