# 0005. The images are defined in the Dagger module, not a Dockerfile

- **Status:** Accepted
- **Date:** 2026-09-23

## Context

The project ships two images: the Operator's, which runs `cmd/main.go`, and
the Exec Agent's, which runs `cmd/exec-agent` on every Host. Both are built
for linux/amd64 and linux/arm64 and published to ghcr.io from CI (#33).

CI is the Dagger module in `.dagger/` (#3). kubebuilder scaffolded a
`Dockerfile`, which `make docker-build` and `make docker-buildx` use, and
which built both binaries into one image. Two definitions of an image drift,
so one of them has to go. The choice was:

- Dagger builds from the `Dockerfile`, which keeps `docker build` as it is;
- or Dagger builds the images itself, and the Makefile's image targets call
  Dagger.

## Decision

1. **The Dagger module defines both images,** in `.dagger/main.go`, and
   the `Dockerfile` and `.dockerignore` are removed.
2. **The Makefile's image targets call Dagger.** `make docker-build` builds
   both images for the engine's platform and loads them into the local
   Docker, as `IMG` and `EXEC_AGENT_IMG`, which is what the kind e2e tests
   need. `make docker-push` builds both platforms and pushes them.
   `make docker-buildx` is removed: `docker-push` is multi-platform.
3. **Each image is one static binary on distroless static, as user
   65532.** The binary is cross-compiled on the engine's platform with
   `CGO_ENABLED=0`, so no emulation is needed for the other platform.
4. **Every base image is pulled from a mirror and pinned by digest:** the
   Go and Rust images from `mirror.gcr.io`, distroless from `gcr.io`. A
   Docker Hub pull has already timed out in CI.
5. **The Exec Agent has its own image,**
   `ghcr.io/phoban01/battery-operator/exec-agent`, below the Operator's
   `ghcr.io/phoban01/battery-operator`, rather than sharing the Operator's
   image under another command.

## Consequences

1. There is one definition of each image, and CI, `dagger call` and `make`
   all use it.
2. Building an image needs the Dagger CLI, which devbox installs, and a
   container runtime for its engine. Plain `docker build .` no longer
   works.
3. The images share the checks' Go image and cache volumes, so a local
   build after `dagger call build` reuses the module and build caches.
4. The Exec Agent's image carries only the Exec Agent, so a Host runs no
   Operator code, and the two can be scanned and rolled out separately.
5. Updating a base image means updating its digest in `.dagger/main.go`.

## Open questions

- Packages that the workflow creates on ghcr.io start private. They have to
  be made public once, by hand, before a cluster can pull them without an
  image pull secret.
