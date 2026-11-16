# Working in this repository

Standing instructions from the repository's owner, recorded so that a new
session starts with them. Design and scope are in `PLAN.md`; how to build
and test is in `CONTRIBUTING.md`.

- **Always monitor CI.** After every push, watch the pull request's checks
  through to the end without being asked: read each failure, fix its root
  cause, and say what happened. A job that never got a runner or was
  cancelled is re-run, not left. (Asked on 2026-11-16.)
- **One pull request open at a time**, large and chunky; fixes go into the
  open one. No fix or bug is deferred.
- **Merging**, when asked to merge once CI is green: squash, a one-line
  title of at most 80 characters, an empty body, no trailers.
- **macOS is tested in CI.** A Linux machine cross-builds and vets only
  (`CONTRIBUTING.md`); what the container library and Seatbelt do is
  proved by the macOS jobs.
- **Kubernetes is held at 1.35**; Linux binaries and Linux-idiom
  Dockerfiles are a non-goal (`PLAN.md`).
