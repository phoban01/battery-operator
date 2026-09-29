package main

// The Manifests as an OCI artifact that Flux's OCIRepository pulls
// (docs/requirements/06-deployment.md#manifests-artifact). The Makefile's
// build-installer target renders them, and hack/manifests-artifact.sh
// pushes them with the flux CLI, which gives the artifact Flux's media
// types and annotations. A pull request checks the artifact against a
// registry inside the check's own container; a version tag pushes it to
// ghcr.io.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dagger/battery-operator/internal/dagger"
)

const (
	// fluxCLIImage is the flux CLI that pushes, tags and pulls the
	// artifact. Its binary is copied out, so the image's own entrypoint
	// and user do not matter.
	fluxCLIImage = "ghcr.io/fluxcd/flux-cli:v2.9.5@sha256:704d5529535570a495249b643159c93a9aa88ffca07813c974a54b7ba6edbd86"
	// registryImage is distribution's registry, which ManifestsCheck runs
	// on the check's loopback address. Its binary is static.
	registryImage = "mirror.gcr.io/library/registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8"
	// manifestsRepository is where ManifestsPublish pushes by default.
	manifestsRepository = "ghcr.io/phoban01/battery-operator/manifests"

	// The images ManifestsCheck renders the Manifests with: the shape of a
	// release's, a tag and a digest. internal/manifests/release_test.go
	// uses the same.
	checkOperatorImage  = "ghcr.io/phoban01/battery-operator:v0.0.0-check@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	checkExecAgentImage = "ghcr.io/phoban01/battery-operator/exec-agent:v0.0.0-check@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// Manifests renders the Manifests as a release ships them, through `make
// build-installer`: config/release, which is config/default and
// config/exec-agent, with the images given, and battery's at the version
// go.mod pins. It returns a directory that holds install.yaml alone, which
// is what the Manifests' OCI artifact holds.
func (m *BatteryOperator) Manifests(
	// The Operator's image. Defaults to the Makefile's IMG.
	// +optional
	operatorImage string,
	// The Exec Agent's image. Defaults to the Makefile's EXEC_AGENT_IMG.
	// +optional
	execAgentImage string,
) *dagger.Directory {
	args := []string{"make", "build-installer"}
	if operatorImage != "" {
		args = append(args, "IMG="+operatorImage)
	}
	if execAgentImage != "" {
		args = append(args, "EXEC_AGENT_IMG="+execAgentImage)
	}
	install := m.gobase("manifests").
		WithExec(args).
		File(src + "/dist/install.yaml")
	return dag.Directory().WithFile("install.yaml", install)
}

// ManifestsPublish renders the Manifests with the images given, which have
// to be pinned by digest, and pushes them as an OCI artifact to
// <repository>:<version>, tagged <commit> and every tag in tags too. It
// returns the artifact's reference, <repository>:<version>@<digest>.
//
// CI runs it on a version tag, with the images Publish pushed at that tag:
//
//	dagger call manifests-publish --version=v0.1.0 --commit=$(git rev-parse HEAD) \
//	  --operator-image=ghcr.io/phoban01/battery-operator:v0.1.0@sha256:... \
//	  --exec-agent-image=ghcr.io/phoban01/battery-operator/exec-agent:v0.1.0@sha256:...
func (m *BatteryOperator) ManifestsPublish(
	ctx context.Context,
	// The version, v<major>.<minor>.<patch>, the artifact's first tag.
	version string,
	// The commit's full SHA, the artifact's second tag.
	commit string,
	// The Operator's image, <repository>:<version>@sha256:<digest>.
	operatorImage string,
	// The Exec Agent's image, <repository>:<version>@sha256:<digest>.
	execAgentImage string,
	// The repository to push to.
	// +default="ghcr.io/phoban01/battery-operator/manifests"
	repository string,
	// More tags.
	// +optional
	tags []string,
	// The registry user. Without one, the push needs no credentials.
	// +optional
	username string,
	// The registry password or token, given with username.
	// +optional
	password *dagger.Secret,
) (string, error) {
	if (username == "") != (password == nil) {
		return "", fmt.Errorf("give both a username and a password, or neither")
	}
	ctr := dag.Container().
		From(goImage).
		WithFile("/usr/local/bin/flux", fluxBinary()).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithDirectory("/artifact", m.Manifests(operatorImage, execAgentImage)).
		// A push always runs: a tag moves, so it may not come from the
		// cache.
		WithEnvVariable("PUSHED_AT", time.Now().UTC().Format(time.RFC3339Nano))
	if username != "" {
		ctr = ctr.
			WithEnvVariable("REGISTRY_USERNAME", username).
			WithSecretVariable("REGISTRY_PASSWORD", password)
	}
	out, err := ctr.
		WithExec(append([]string{"hack/manifests-artifact.sh", "push", "/artifact", repository, version, commit}, tags...)).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out) + "\n", nil
}

// ManifestsCheck renders the Manifests with images pinned as a release
// pins them, pushes them with hack/manifests-artifact.sh push to a
// registry in the check's own container, and checks what it pushed, as
// hack/manifests-artifact-check.sh says: the media types, the annotations,
// both tags, and what `flux pull artifact` extracts. It pushes nothing
// outside the container, so a pull request runs it.
func (m *BatteryOperator) ManifestsCheck(ctx context.Context) (string, error) {
	return m.gobase("manifests-check").
		WithFile("/usr/local/bin/flux", fluxBinary()).
		WithFile("/usr/local/bin/registry", dag.Container().From(registryImage).File("/bin/registry")).
		WithDirectory("/artifact", m.Manifests(checkOperatorImage, checkExecAgentImage)).
		WithEnvVariable("IMG", checkOperatorImage).
		WithEnvVariable("EXEC_AGENT_IMG", checkExecAgentImage).
		WithExec([]string{"sh", "-ec", `
make kustomize >/dev/null
KUSTOMIZE="$PWD/bin/kustomize" hack/manifests-artifact-check.sh /artifact 2>&1
`}).
		Stdout(ctx)
}

// fluxBinary is the flux CLI of fluxCLIImage, a static binary.
func fluxBinary() *dagger.File {
	return dag.Container().From(fluxCLIImage).File("/usr/local/bin/flux")
}
