{
  lib,
  rustPlatform,
  fetchFromGitHub,
  fetchurl,
  nixosTests,
  nix-update-script,
  oniguruma,
  pkg-config,
  rust-jemalloc-sys-unprefixed,
  zstd,
}:

rustPlatform.buildRustPackage (finalAttrs: {
  pname = "meilisearch";
  version = "1.57.0";

  src = fetchFromGitHub {
    owner = "meilisearch";
    repo = "meilisearch";
    tag = "v${finalAttrs.version}";
    hash = "sha256-Vmzl6kiI7/pZn/RiBfJY1x7HqTGDeNh3Xt40PnkkfAE=";
  };

  # meilitool too, which the release's image carries.
  cargoBuildFlags = [
    "--package=meilisearch"
    "--package=meilitool"
  ];

  cargoHash = "sha256-Lnp+7k4ZEwkcVNWHJMBAbqJevmaF/P9Wn1+E30Xn+f0=";

  # The release's default features: every tokenization and the
  # mini-dashboard. Their build scripts download what they embed: the
  # dashboard's assets, which the build script reads from the fetched
  # archive, checked against the SHA-1 Cargo.toml pins as a download is,
  # and lindera's Japanese and Korean dictionaries, which it takes from
  # its build cache, checked against their MD5s as a download is.
  env.MEILI_MINI_DASHBOARD_ZIP = fetchurl {
    url = "https://github.com/meilisearch/mini-dashboard/releases/download/v0.4.2/build.zip";
    hash = "sha256-I5rWwcR1RK1de9PWSIFPJOpnl0ZrB938rEo1I2Wayms=";
  };
  preBuild = let
    dictionary = name: hash: fetchurl { url = "https://lindera.dev/${name}"; inherit hash; };
    unidic = dictionary "unidic-mecab-2.1.2.tar.gz" "sha256-JKx1/k5E2XO1XmWEfDX6Suwtt6QaB7ScoSUUbbn8EYk=";
    kodic = dictionary "mecab-ko-dic-2.1.1-20180720.tar.gz" "sha256-cCztIcYWfp2a68Z0q17lSvWNREOXXylA030FZ8AgWRo=";
  in ''
    export LINDERA_BUILD_DICTIONARY_CACHE_DIR="$TMPDIR/lindera"
    mkdir -p "$LINDERA_BUILD_DICTIONARY_CACHE_DIR/5.3.0-fmt2"
    cp ${unidic} "$LINDERA_BUILD_DICTIONARY_CACHE_DIR/5.3.0-fmt2/unidic-mecab-2.1.2.tar.gz"
    cp ${kodic} "$LINDERA_BUILD_DICTIONARY_CACHE_DIR/5.3.0-fmt2/mecab-ko-dic-2.1.1-20180720.tar.gz"
  '';
  # The build script's zip takes flate2 without a backend, which the
  # tokenizers' build scripts give it in the release's build: flate2's
  # default backend, a crate Cargo.lock holds already, is named here.
  postPatch = ''
    substituteInPlace crates/meilisearch/Cargo.toml \
      --replace-fail 'zip = { version = "6.0.0", optional = true, default-features = false, features = ["deflate-flate2"] }' \
        'zip = { version = "6.0.0", optional = true, default-features = false, features = ["deflate-flate2"] }
    flate2 = "1.1.9"'
    substituteInPlace crates/meilisearch/build.rs \
      --replace-fail 'let dashboard_assets_bytes = get(url)?.bytes()?;' \
        'let _ = (url, get::<&str>); let dashboard_assets_bytes = std::fs::read(env::var("MEILI_MINI_DASHBOARD_ZIP")?)?;'
  '';

  nativeBuildInputs = [
    pkg-config
    rustPlatform.bindgenHook
  ];

  # zstd for zstd-sys, which the default features link and nixpkgs'
  # Rust builds point at the system's library.
  buildInputs = [
    oniguruma
    rust-jemalloc-sys-unprefixed
    zstd
  ];

  passthru = {
    updateScript = nix-update-script { extraArgs = [ "--use-github-releases" ]; };
    tests = {
      meilisearch = nixosTests.meilisearch;
    };
  };

  # Tests will try to compile with mini-dashboard features which downloads something from the internet.
  doCheck = false;

  meta = {
    description = "Powerful, fast, and an easy to use search engine";
    mainProgram = "meilisearch";
    homepage = "https://docs.meilisearch.com/";
    changelog = "https://github.com/meilisearch/meilisearch/releases/tag/v${finalAttrs.version}";
    license = lib.licenses.mit;
    maintainers = with lib.maintainers; [
      happysalada
      bbenno
    ];
    platforms = [
      "aarch64-linux"
      "aarch64-darwin"
      "riscv64-linux"
      "x86_64-linux"
    ];
  };
})
