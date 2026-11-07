# The dashboard Qdrant's image serves from /qdrant/static: Qdrant's own
# image fetches the web UI's latest release when it builds, the catalog a
# pinned one.
{ nixpkgs, system ? builtins.currentSystem }:
(builtins.getFlake nixpkgs).legacyPackages.${system}.fetchzip {
  url = "https://github.com/qdrant/qdrant-web-ui/releases/download/v0.2.20/dist-qdrant.zip";
  hash = "sha256-UPHOZj8I4rVXRRBpVr9qsjXOlf36vHh+PH14fruJmR0=";
}
