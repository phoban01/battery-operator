// CI for battery-operator: lint, test, build, generated files and requirements
//
// Each function runs the repository's own make targets in a container with
// the Go that go.mod pins, so CI and a local `dagger call` use the same
// Makefile and the same tool versions. The Go module and build caches, and
// the tools the Makefile installs into bin/, live in cache volumes.
//
// Run them from the repository root, for example:
//
//	dagger call lint
//	dagger call test
//	dagger call build export --path=bin/manager
//	dagger call check-generated
//	dagger call requirements --owns=none
//	dagger call duvet-report export --path=.duvet/reports
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/battery-operator/internal/dagger"
)

const (
	// goImage matches the go directive in go.mod and devbox.json's go.
	goImage = "golang:1.26.5-trixie@sha256:f1a132429b98724a904e9b3bdbaed399d8f923203c3e5170e6def66d0a7cc04c"
	// rustImage builds duvet. It is on the same Debian release as goImage,
	// so the binary runs there.
	rustImage = "rust:1.97.1-trixie@sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f"
	// duvetVersion matches DUVET_VERSION in devbox.json.
	duvetVersion = "0.4.3"

	src = "/src"
)

type BatteryOperator struct {
	// The repository.
	Source *dagger.Directory
}

func New(
	// The repository. Defaults to the one this module is in.
	// +defaultPath="/"
	// +ignore=[".git", ".gopath", ".devbox", ".dagger", "bin", "cover.out", "dist", ".duvet/reports", ".duvet/requirements"]
	source *dagger.Directory,
) *BatteryOperator {
	return &BatteryOperator{Source: source}
}

// Lint runs golangci-lint through `make lint`.
func (m *BatteryOperator) Lint(ctx context.Context) (string, error) {
	return m.gobase("lint").
		WithMountedCache("/root/.cache/golangci-lint", dag.CacheVolume("battery-operator-golangci-lint")).
		WithExec([]string{"make", "lint"}).
		Stdout(ctx)
}

// Test runs the unit and envtest tests through `make test`, which downloads
// the envtest binaries itself.
func (m *BatteryOperator) Test(ctx context.Context) (string, error) {
	return m.gobase("test").
		WithExec([]string{"make", "test"}).
		Stdout(ctx)
}

// Build builds the manager through `make build` and returns the binary.
func (m *BatteryOperator) Build() *dagger.File {
	return m.gobase("build").
		WithExec([]string{"make", "build"}).
		// bin/ is a cache volume; copy the binary out of it.
		WithExec([]string{"cp", "bin/manager", "/manager"}).
		File("/manager")
}

// CheckGenerated fails if `make generate manifests` changes the tree.
func (m *BatteryOperator) CheckGenerated(ctx context.Context) (string, error) {
	return m.gobase("generate").
		WithExec([]string{"sh", "-ec", `
git init -q
git add -A
git -c user.name=ci -c user.email=ci@localhost commit -q --no-verify -m before
make generate manifests
if [ -n "$(git status --porcelain)" ]; then
  echo "make generate manifests changed the tree; run it and commit the result:" >&2
  git status --short >&2
  git --no-pager diff >&2
  exit 1
fi
echo "generated files are up to date"
`}).
		Stdout(ctx)
}

// DuvetReport runs `make duvet` and returns .duvet/reports, the HTML and
// JSON report.
func (m *BatteryOperator) DuvetReport() *dagger.Directory {
	return m.duvet().Directory(src + "/.duvet/reports")
}

// Requirements runs `make duvet`, then the coverage gate over the
// requirement IDs in owns, the value of a PR body's Owns: line. "none" skips
// the gate, for a PR that implements no requirement.
func (m *BatteryOperator) Requirements(
	ctx context.Context,
	// The Owns: line's value: requirement IDs separated by spaces or commas,
	// or "none".
	owns string,
) (string, error) {
	ids := strings.Fields(strings.ReplaceAll(owns, ",", " "))
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("no requirement IDs given; a PR that implements no requirement says 'Owns: none'")
	case len(ids) == 1 && ids[0] == "none":
		// Still produce the report, so a broken spec or citation fails.
		if _, err := m.duvet().Sync(ctx); err != nil {
			return "", err
		}
		return "no requirement IDs claimed; skipping the coverage gate\n", nil
	}
	return m.duvet().
		WithEnvVariable("SKIP_REPORT", "1").
		WithExec(append([]string{"hack/duvet-coverage.sh"}, ids...)).
		Stdout(ctx)
}

// gobase is the Go image with the source at /src and the caches mounted.
// Each function has its own bin/ volume, so that two functions running at
// once do not install the same tool into the same directory.
func (m *BatteryOperator) gobase(name string) *dagger.Container {
	return dag.Container().
		From(goImage).
		WithEnvVariable("GOTOOLCHAIN", "local").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("battery-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("battery-operator-go-build")).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithMountedCache(src+"/bin", dag.CacheVolume("battery-operator-bin-"+name))
}

// duvet is the Go image with duvet and jq, after `make duvet`.
func (m *BatteryOperator) duvet() *dagger.Container {
	return dag.Container().
		From(goImage).
		WithExec([]string{"sh", "-ec", "apt-get update -qq && apt-get install -y -qq --no-install-recommends jq >/dev/null"}).
		WithFile("/usr/local/bin/duvet", duvetBinary()).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithExec([]string{"make", "duvet"})
}

// duvetBinary builds duvet at duvetVersion with cargo, the way devbox does.
func duvetBinary() *dagger.File {
	return dag.Container().
		From(rustImage).
		WithMountedCache("/usr/local/cargo/registry", dag.CacheVolume("battery-operator-cargo-registry")).
		WithMountedCache("/cargo-target", dag.CacheVolume("battery-operator-cargo-target-duvet-"+duvetVersion)).
		WithEnvVariable("CARGO_TARGET_DIR", "/cargo-target").
		WithExec([]string{"cargo", "install", "duvet", "--version", duvetVersion, "--locked", "--root", "/duvet"}).
		File("/duvet/bin/duvet")
}
