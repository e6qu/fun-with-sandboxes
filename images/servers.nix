# The catalog's database servers, built with the compilation comment their
# vendors' builds carry, which the version comment and the "ready for
# connections" line report and clients such as Testcontainers wait for.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  # The comment holds spaces, which cmakeFlags would split on.
  commented = pkg: comment: pkg.overrideAttrs (old: {
    preConfigure = (old.preConfigure or "") + ''
      cmakeFlagsArray+=("-DCOMPILATION_COMMENT=${comment}")
    '';
  });
in
{
  mysql = commented pkgs.mysql84 "MySQL Community Server - GPL";
  mariadb = commented pkgs.mariadb "mariadb.org binary distribution";
}
