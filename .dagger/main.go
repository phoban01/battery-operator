// CI for battery-operator: lint, test, build, generated files, requirements,
// images and the Quint models
//
// The checks run the repository's own make targets in a container with the
// Go that go.mod pins, so CI and a local `dagger call` use the same Makefile
// and the same tool versions. The Go module and build caches, and the tools
// the Makefile installs into bin/, live in cache volumes. The image
// functions build with the same Go, and the Makefile's image targets call
// them.
//
// Run them from the repository root, for example:
//
//	dagger call lint
//	dagger call test
//	dagger call build export --path=bin/manager
//	dagger call check-generated
//	dagger call requirements --owns=none
//	dagger call duvet-report export --path=.duvet/reports
//	dagger call quint
//	dagger call images export --path=dist/images
//	dagger call operator-image export-image --name=battery-operator:dev
//	dagger call fake-flintlockd-image export-image --name=fake-flintlockd:dev
//	dagger call publish --repository=ttl.sh/battery-operator-dev --tags=1h
//
// The two images, the Operator's and the Exec Agent's, and the e2e suite's
// fake flintlockd image are defined here and nowhere else. There is no
// Dockerfile: see
// docs/adr/0005-images-built-by-dagger.md.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/battery-operator/internal/dagger"
)

// Base images come from mirrors, not Docker Hub, whose pulls have timed out
// in CI. Each is pinned by the digest of its multi-platform index.
// mirror.gcr.io serves Docker Hub's library images with Docker Hub's
// digests.
const (
	// goImage matches the go directive in go.mod and devbox.json's go.
	goImage = "mirror.gcr.io/library/golang:1.26.5-trixie@sha256:f1a132429b98724a904e9b3bdbaed399d8f923203c3e5170e6def66d0a7cc04c"
	// rustImage builds duvet. It is on the same Debian release as goImage,
	// so the binary runs there.
	rustImage = "mirror.gcr.io/library/rust:1.97.1-trixie@sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f"
	// runtimeImage is the base of both images: static, with no shell, as
	// kubebuilder's scaffold had it.
	runtimeImage = "gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
	// nonroot is runtimeImage's non-root user, by number, so that the
	// kubelet can verify runAsNonRoot.
	nonroot = "65532:65532"
	// duvetVersion matches DUVET_VERSION in devbox.json.
	duvetVersion = "0.4.3"
	// quintVersion matches quint in devbox.json, and quintEvaluatorVersion is
	// the Rust evaluator that quint release uses for `quint run`.
	quintVersion          = "0.32.0"
	quintEvaluatorVersion = "0.6.0"
	quintHome             = "/opt/quint"

	src = "/src"

	// sourceURL labels the images, which links the packages on ghcr.io to
	// the repository.
	sourceURL = "https://github.com/phoban01/battery-operator"
)

// platforms are what every image is built and published for.
var platforms = []dagger.Platform{"linux/amd64", "linux/arm64"}

// component is one of the two images.
type component struct {
	image       string // the image's name, in the tarball Images returns
	binary      string // the binary, at /<binary> in the image
	pkg         string // the binary's main package
	suffix      string // appended to the repository Publish is given
	description string
}

var (
	operator = component{
		image:       "operator",
		binary:      "manager",
		pkg:         "./cmd",
		description: "battery-operator: the Operator's manager",
	}
	execAgent = component{
		image:       "exec-agent",
		binary:      "exec-agent",
		pkg:         "./cmd/exec-agent",
		suffix:      "/exec-agent",
		description: "battery-operator: the Exec Agent, which runs on every Host",
	}
	components = []component{operator, execAgent}

	// fakeFlintlockd is the fake flintlockd (cmd/fake-flintlockd), which the
	// e2e suite runs on every kind node that is a Host. It is a test double,
	// so it is not in components: Images and Publish leave it out.
	fakeFlintlockd = component{
		image:       "fake-flintlockd",
		binary:      "fake-flintlockd",
		pkg:         "./cmd/fake-flintlockd",
		description: "battery-operator: the e2e suite's fake flintlockd, a test double",
	}
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

// Quint typechecks, tests and simulates the Quint models in specs/quint
// through `make quint`, checking every model's invariants.
func (m *BatteryOperator) Quint(ctx context.Context) (string, error) {
	return dag.Container().
		From(goImage).
		WithExec([]string{"sh", "-ec", installQuint}).
		WithEnvVariable("QUINT_HOME", quintHome).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithExec([]string{"make", "quint"}).
		Stdout(ctx)
}

// installQuint installs quint's release binary and the Rust evaluator that
// `quint run` uses, both checked against their release checksums. quint
// would otherwise download the evaluator itself, unchecked, on first use;
// it looks for it under $QUINT_HOME.
var installQuint = fmt.Sprintf(`
case "$(dpkg --print-architecture)" in
  amd64) arch=amd64; triple=x86_64-unknown-linux-gnu; quint_sum=%[3]s; eval_sum=%[4]s ;;
  arm64) arch=arm64; triple=aarch64-unknown-linux-gnu; quint_sum=%[5]s; eval_sum=%[6]s ;;
  *) echo "no quint release for $(dpkg --print-architecture)" >&2; exit 1 ;;
esac
base=https://github.com/informalsystems/quint/releases/download
curl -fsSLo /usr/local/bin/quint "$base/v%[1]s/quint-linux-$arch"
echo "$quint_sum  /usr/local/bin/quint" | sha256sum -c -
chmod +x /usr/local/bin/quint
curl -fsSLo /tmp/evaluator.tar.gz "$base/evaluator/v%[2]s/quint_evaluator-$triple.tar.gz"
echo "$eval_sum  /tmp/evaluator.tar.gz" | sha256sum -c -
mkdir -p %[7]s/rust-evaluator-v%[2]s
tar -xzf /tmp/evaluator.tar.gz -C %[7]s/rust-evaluator-v%[2]s
rm /tmp/evaluator.tar.gz
`,
	quintVersion, quintEvaluatorVersion,
	// sha256 of quint-linux-amd64 and quint_evaluator-x86_64-unknown-linux-gnu.tar.gz
	"939b64095b706017f2f202c6f99c860c40be7c31bddc2b98557316e50f42cd7f",
	"61755a09d5052d93a4e75e840059edfd0d3674aeda164b9d2464be3d6e21b1c2",
	// sha256 of quint-linux-arm64 and quint_evaluator-aarch64-unknown-linux-gnu.tar.gz
	"5b23e6f7e6f6b9c870c5ea7d38675e8fc709f4578bcf4a236918414157267a35",
	"07e5ec9c756feba0db59987c9f90456dacfecf75f64f111b791b66c770a293d2",
	quintHome,
)

// OperatorImage builds the Operator's image, which runs /manager, for one
// platform.
func (m *BatteryOperator) OperatorImage(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. Defaults to the engine's.
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	return m.image(ctx, operator, platform)
}

// ExecAgentImage builds the Exec Agent's image, which runs /exec-agent, for
// one platform.
func (m *BatteryOperator) ExecAgentImage(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. Defaults to the engine's.
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	return m.image(ctx, execAgent, platform)
}

// FakeFlintlockdImage builds the fake flintlockd's image, which runs
// /fake-flintlockd, for one platform. The e2e suite runs it as each kind
// Host's flintlockd. It is never published.
func (m *BatteryOperator) FakeFlintlockdImage(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. Defaults to the engine's.
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	return m.image(ctx, fakeFlintlockd, platform)
}

// Images builds both images for linux/amd64 and linux/arm64, and returns
// them as multi-platform OCI tarballs: operator.tar and exec-agent.tar.
func (m *BatteryOperator) Images(ctx context.Context) (*dagger.Directory, error) {
	dir := dag.Directory()
	for _, c := range components {
		variants, err := m.variants(ctx, c)
		if err != nil {
			return nil, err
		}
		dir = dir.WithFile(c.image+".tar",
			dag.Container().AsTarball(dagger.ContainerAsTarballOpts{PlatformVariants: variants}))
	}
	return dir, nil
}

// Publish builds both images for linux/amd64 and linux/arm64, and pushes
// each at every tag: the Operator's to <repository>:<tag> and the Exec
// Agent's to <repository>/exec-agent:<tag>. It returns the references it
// pushed, with their digests.
func (m *BatteryOperator) Publish(
	ctx context.Context,
	// The tags, for example a commit SHA, latest, or v0.1.0.
	tags []string,
	// The Operator's repository. The Exec Agent's is below it.
	// +default="ghcr.io/phoban01/battery-operator"
	repository string,
	// The registry user. Without one, the engine uses the client's
	// credentials, such as a `docker login`'s.
	// +optional
	username string,
	// The registry password or token, given with username.
	// +optional
	password *dagger.Secret,
) (string, error) {
	if len(tags) == 0 {
		return "", fmt.Errorf("no tags given")
	}
	if (username == "") != (password == nil) {
		return "", fmt.Errorf("give both a username and a password, or neither")
	}
	registry, _, _ := strings.Cut(repository, "/")

	var out strings.Builder
	for _, c := range components {
		variants, err := m.variants(ctx, c)
		if err != nil {
			return "", err
		}
		for _, tag := range tags {
			ctr := dag.Container()
			if username != "" {
				ctr = ctr.WithRegistryAuth(registry, username, password)
			}
			ref, err := ctr.Publish(ctx, repository+c.suffix+":"+tag,
				dagger.ContainerPublishOpts{PlatformVariants: variants})
			if err != nil {
				return "", fmt.Errorf("publishing the %s image: %w", c.image, err)
			}
			fmt.Fprintln(&out, ref)
		}
	}
	return out.String(), nil
}

// variants is c's image for each of platforms.
func (m *BatteryOperator) variants(ctx context.Context, c component) ([]*dagger.Container, error) {
	variants := make([]*dagger.Container, 0, len(platforms))
	for _, p := range platforms {
		ctr, err := m.image(ctx, c, p)
		if err != nil {
			return nil, err
		}
		variants = append(variants, ctr)
	}
	return variants, nil
}

// image is c's image for platform: c's binary alone on runtimeImage, run as
// nonroot. An empty platform is the engine's. The binary is cross-compiled
// on the engine's platform, so no emulation is needed.
func (m *BatteryOperator) image(ctx context.Context, c component, platform dagger.Platform) (*dagger.Container, error) {
	if platform == "" {
		var err error
		if platform, err = dag.DefaultPlatform(ctx); err != nil {
			return nil, err
		}
	}
	goos, goarch, _ := strings.Cut(string(platform), "/")
	if goos != "linux" || goarch == "" || strings.Contains(goarch, "/") {
		return nil, fmt.Errorf("platform %q: want linux/<arch>, for example linux/arm64", platform)
	}
	out := "/out/" + c.binary
	binary := m.gobase("image").
		WithEnvVariable("CGO_ENABLED", "0").
		WithEnvVariable("GOOS", goos).
		WithEnvVariable("GOARCH", goarch).
		WithExec([]string{"go", "build", "-trimpath", "-o", out, c.pkg}).
		File(out)
	return dag.Container(dagger.ContainerOpts{Platform: platform}).
		From(runtimeImage).
		WithFile("/"+c.binary, binary).
		WithWorkdir("/").
		WithUser(nonroot).
		WithEntrypoint([]string{"/" + c.binary}).
		WithLabel("org.opencontainers.image.source", sourceURL).
		WithLabel("org.opencontainers.image.description", c.description).
		WithLabel("org.opencontainers.image.licenses", "Apache-2.0"), nil
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
