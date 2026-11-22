{
  buildGoModule,
  fetchFromGitHub,
  fetchurl,
  fetchpatch,
  go-bindata,
  lib,
  perl,
  pkg-config,
  rustPlatform,
  stdenv,
  libiconv,
  nixosTests,
}:

let
  version = "2.10.1";
  ui_version = "OSS-v2.10.0";
  libflux_version = "0.201.0";

  src = fetchFromGitHub {
    owner = "influxdata";
    repo = "influxdb";
    rev = "v${version}";
    hash = "sha256-0tPCNrXh5wyYtUH5agXGa2arVpd6XUWcfqhKjr2kIcc=";
  };

  ui = fetchurl {
    url = "https://github.com/influxdata/ui/releases/download/${ui_version}/build.tar.gz";
    hash = "sha256-X3E8ZoAP+BihWrRQsABK4MdOFwxjjZABkW6DJNV9F6k=";
  };

  flux = rustPlatform.buildRustPackage (finalAttrs: {
    pname = "libflux";
    version = libflux_version;
    src = fetchFromGitHub {
      owner = "influxdata";
      repo = "flux";
      rev = "v${libflux_version}";
      hash = "sha256-gcQ+nPgvgdwokRmL9QPyjoDcUmtTPKhPvLKcWnGj2lQ=";
    };
    # Don't fail on warnings
    postPatch = ''
      substituteInPlace flux/Cargo.toml \
        --replace-fail 'default = ["strict", ' 'default = ['
      substituteInPlace flux-core/Cargo.toml \
        --replace-fail 'default = ["strict"]' 'default = []'
    '';
    sourceRoot = "${finalAttrs.src.name}/libflux";

    cargoHash = "sha256-hSQDiFvVsFrRccC1yFV2+bmRzch6VnwChrkTW93L1ws=";
    nativeBuildInputs = [ rustPlatform.bindgenHook ];
    buildInputs = lib.optional stdenv.hostPlatform.isDarwin libiconv;
    pkgcfg = ''
      Name: flux
      Version: ${libflux_version}
      Description: Library for the InfluxData Flux engine
      Cflags: -I/out/include
      Libs: -L/out/lib -lflux -lpthread
    '';
    postInstall = ''
      mkdir -p $out/include $out/pkgconfig
      cp -r $NIX_BUILD_TOP/${finalAttrs.src.name}/libflux/include/influxdata $out/include
      printf "%s" "$pkgcfg" > $out/pkgconfig/flux.pc
      substituteInPlace $out/pkgconfig/flux.pc \
        --replace-fail /out $out
    ''
    + lib.optionalString stdenv.hostPlatform.isDarwin ''
      install_name_tool -id $out/lib/libflux.dylib $out/lib/libflux.dylib
    '';

    __structuredAttrs = true;
  });
in
buildGoModule {
  pname = "influxdb";
  version = version;
  inherit src;

  nativeBuildInputs = [
    go-bindata
    pkg-config
    perl
  ];

  vendorHash = "sha256-NY0R9YUeJX2M3jtMWAACtjPY55N2vQb+bDkoypjmrM0=";
  subPackages = [
    "cmd/influxd"
    "cmd/telemetryd"
  ];

  env.PKG_CONFIG_PATH = "${flux}/pkgconfig";

  postPatch = ''
    # use go-bindata from environment
    substituteInPlace static/static.go \
      --replace-fail 'go run github.com/kevinburke/go-bindata/' ""
  '';

  # Check that libflux and the UI are at the right version, and embed
  # the UI assets into the Go source tree.
  preBuild = ''
    (
      flux_ver=$(grep github.com/influxdata/flux go.mod | awk '{print $2}')
      if [ "$flux_ver" != "v${libflux_version}" ]; then
        echo "go.mod wants libflux $flux_ver, but nix derivation provides ${libflux_version}"
        exit 1
      fi

      ui_ver=$(egrep 'UI_RELEASE=".*"' scripts/fetch-ui-assets.sh | cut -d'"' -f2)
      if [ "$ui_ver" != "${ui_version}" ]; then
        echo "scripts/fetch-ui-assets.sh wants UI $ui_ver, but nix derivation provides ${ui_version}"
        exit 1
      fi
    )

    mkdir -p static/data
    tar -xzf ${ui} -C static/data
    pushd static
    go generate
    popd
  '';

  tags = [ "assets" ];

  # The vendored modules need no UI assets, and go generate, which embeds
  # them, would load the package before its modules are vendored.
  overrideModAttrs = _: { preBuild = ""; };

  ldflags = [
    "-X main.commit=v${version}"
    "-X main.version=${version}"
  ];

  passthru.tests = {
    inherit (nixosTests) influxdb2;
  };

  meta = {
    description = "Open-source distributed time series database";
    license = lib.licenses.mit;
    homepage = "https://influxdata.com/";
    maintainers = [ ];
  };
}
