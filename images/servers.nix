# The catalog's database servers at their series' newest releases, built
# with the compilation comment their vendors' builds carry, which the
# version comment and the "ready for connections" line report and clients
# such as Testcontainers wait for.
{ nixpkgs, system ? builtins.currentSystem }:
let
  pkgs = (builtins.getFlake nixpkgs).legacyPackages.${system};
  # The comment holds spaces, which cmakeFlags would split on.
  commented = pkg: comment: pkg.overrideAttrs (old: {
    preConfigure = (old.preConfigure or "") + ''
      cmakeFlagsArray+=("-DCOMPILATION_COMMENT=${comment}")
    '';
  });
  # The newest release of the series, which nixpkgs may not package yet.
  release = pkg: version: url: hash: pkg.overrideAttrs {
    inherit version;
    src = pkgs.fetchurl { inherit url hash; };
  };
in
{
  mysql = commented (release pkgs.mysql84 "8.4.13"
    "https://dev.mysql.com/get/Downloads/MySQL-8.4/mysql-8.4.13.tar.gz"
    "sha256-9t3H9/tkYrq4tTOxPAavP/9X+pyhIlm4GrE82ZMLnVg=") "MySQL Community Server - GPL";
  mariadb = commented (release pkgs.mariadb "11.4.14"
    "https://archive.mariadb.org/mariadb-11.4.14/source/mariadb-11.4.14.tar.gz"
    "sha256-UpUCWDXaRJhxUYYG9QwRNAR9+XjkOTnkVTc33QeOEmE=") "mariadb.org binary distribution";
}
