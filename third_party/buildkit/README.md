# BuildKit v0.34.1 (vendored subset)

These are the packages of [BuildKit](https://github.com/moby/buildkit)
v0.34.1 that `internal/buildkit` uses, copied by
`scripts/sync-buildkit.sh` under BuildKit's Apache License 2.0 (see
`LICENSE` and `NOTICE`).

Changes from upstream:

- Import paths point into this module.
- The patches in `third_party/buildkit-patches` are applied:
  - `localmounter_darwin.patch`
  - `resolvecache_race.patch`
