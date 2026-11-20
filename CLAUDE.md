# Working in this repository

Standing instructions from the repository's owner, recorded so that a new
session starts with them. Design and scope are in `PLAN.md`; how to build
and test is in `CONTRIBUTING.md`.

- **Always monitor CI.** After every push, watch the pull request's checks
  through to the end without being asked: read each failure, fix its root
  cause, and say what happened. A job that never got a runner or was
  cancelled is re-run, not left. After a merge, watch the run on `main`
  the same way, to the end of its `publish` job: a red run on `main` is
  fixed in the next pull request at once. (Asked on 2026-11-16.)
- **One pull request open at a time**, large and chunky; fixes go into the
  open one. No fix or bug is deferred.
- **Boy scout rule.** Fix every issue met on the way, related to the task or
  not: a flaky test is a bug, fixed at its root, never re-run until it
  passes and left. (Asked on 2026-11-17.)
- **Merging**, when asked to merge once CI is green: squash, a one-line
  title of at most 80 characters, an empty body, no trailers.
- **macOS is tested in CI.** A Linux machine cross-builds and vets only
  (`CONTRIBUTING.md`); what the container library and Seatbelt do is
  proved by the macOS jobs.
- **Dockerfiles built `FROM` fws's base images are in scope**, as far as
  macOS can run their steps: fws provides the darwin base images; consuming
  Linux-specific Dockerfiles is not a goal. Linux binaries are a non-goal,
  as is packaging for distribution (`PLAN.md`). (Asked on 2026-11-20.)
- **Latest available versions.** Catalog images and Kubernetes follow the
  newest release available; where nixpkgs lags, build the release here
  rather than wait. (Asked on 2026-11-20.)
