package main

// The Host Image (hostimage/, docs/requirements/11-host-image.md): a bootc
// image built from its own Containerfile, for linux/amd64 only
// (docs/adr/0007-reference-host-image-and-cluster-api.md).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"dagger/battery-operator/internal/dagger"
)

// craneImage copies the Host Image's base to its mirror
// (MirrorHostImageBase). crane copies an index and its images unchanged.
// The debug variant has a shell, which the registry login needs.
const craneImage = "gcr.io/go-containerregistry/crane/debug:v0.22.1@sha256:e78770b31258a3846f878036d9c1f63fbe4c871f9f56990bf77fd95c013e3c1b"

// digestRE matches a sha256 digest.
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// HostImage builds the Host Image from hostimage/Containerfile for
// linux/amd64. The build runs the image's check stage, and fails when a
// check fails: the image takes a file that only the check stage writes.
// Every *_VERSION of hostimage/versions.env is passed as a build argument,
// which the Containerfile turns into an OCI label.
func (m *BatteryOperator) HostImage(ctx context.Context) (*dagger.Container, error) {
	dir := m.Source.Directory(hostImageDir)
	versions, err := dir.File("versions.env").Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading %s/versions.env: %w", hostImageDir, err)
	}
	args, err := hostImageBuildArgs(versions)
	if err != nil {
		return nil, err
	}
	return dir.DockerBuild(dagger.DirectoryDockerBuildOpts{
		Dockerfile: "Containerfile",
		Platform:   hostImagePlatform,
		BuildArgs:  args,
	}), nil
}

// HostImageCheck builds the Host Image, runs its check stage again inside
// the built image, and then the half of the checks that cannot run inside
// it, hostimage/check-labels.sh: that the image's OCI labels are the
// versions of hostimage/versions.env, that it is linux/amd64, and that the
// versions file in it is that file.
func (m *BatteryOperator) HostImageCheck(ctx context.Context) (string, error) {
	img, err := m.HostImage(ctx)
	if err != nil {
		return "", err
	}
	checks, err := img.WithExec([]string{"/usr/libexec/battery/check"}).Stdout(ctx)
	if err != nil {
		return checks, fmt.Errorf("the check stage, run again in the built image: %w", err)
	}

	labels, err := img.Labels(ctx)
	if err != nil {
		return checks, fmt.Errorf("reading the Host Image's labels: %w", err)
	}
	var lines []string
	for _, l := range labels {
		name, err := l.Name(ctx)
		if err != nil {
			return checks, err
		}
		value, err := l.Value(ctx)
		if err != nil {
			return checks, err
		}
		lines = append(lines, name+"="+value)
	}
	sort.Strings(lines)
	platform, err := img.Platform(ctx)
	if err != nil {
		return checks, fmt.Errorf("reading the Host Image's platform: %w", err)
	}
	facts := dag.Directory().
		WithNewFile("labels", strings.Join(lines, "\n")+"\n").
		WithNewFile("platform", string(platform)+"\n").
		WithFile("versions.env", img.File("/usr/share/battery/versions.env"))

	out, err := dag.Container().
		From(goImage).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithDirectory("/host-image", facts).
		WithExec([]string{"hostimage/check-labels.sh", "/host-image"}).
		Stdout(ctx)
	return checks + out, err
}

// HostImageLint lints the Host Image's sources without building it,
// through hostimage/lint.sh: bash -n and shellcheck over every script, the
// base image's digest pin, the versions file, the check cases the check
// stage runs, the nft syntax of the rendered firewall, and systemd-analyze
// verify. It runs with root capabilities in its container, so that nft can
// check the ruleset against a kernel, and the cases can load the rules into
// network namespaces of their own and show what the kernel enforces.
func (m *BatteryOperator) HostImageLint(ctx context.Context) (string, error) {
	return dag.Container().
		From(goImage).
		WithExec([]string{"sh", "-ec", `
apt-get update -qq
apt-get install -y -qq --no-install-recommends shellcheck nftables iproute2 util-linux systemd >/dev/null
shellcheck --version | sed -n 2p
nft --version
`}).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithEnvVariable("HOST_IMAGE_REQUIRE_SHELLCHECK", "1").
		WithEnvVariable("HOST_IMAGE_REQUIRE_NFT", "1").
		WithExec([]string{"hostimage/lint.sh"}, dagger.ContainerWithExecOpts{InsecureRootCapabilities: true}).
		Stdout(ctx)
}

// HostImagePublish builds the Host Image, its check stage included, and
// pushes it at every tag to repository. It returns the references it
// pushed, with their digests.
func (m *BatteryOperator) HostImagePublish(
	ctx context.Context,
	// The tags, for example a commit SHA or v0.1.0.
	tags []string,
	// The repository.
	// +default="ghcr.io/phoban01/battery-operator/host-image"
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
	img, err := m.HostImage(ctx)
	if err != nil {
		return "", err
	}
	if username != "" {
		registry, _, _ := strings.Cut(repository, "/")
		img = img.WithRegistryAuth(registry, username, password)
	}
	var out strings.Builder
	for _, tag := range tags {
		ref, err := img.Publish(ctx, repository+":"+tag)
		if err != nil {
			return out.String(), fmt.Errorf("publishing the Host Image: %w", err)
		}
		fmt.Fprintln(&out, ref)
	}
	return out.String(), nil
}

// hostImageBuildArgs is a build argument for every KEY_VERSION=value line
// of the versions file. The file is KEY=value lines and comments only
// (hostimage/lint.sh checks that), so it is parsed, not sourced.
func hostImageBuildArgs(versions string) ([]dagger.BuildArg, error) {
	var args []dagger.BuildArg
	for _, line := range strings.Split(versions, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s/versions.env: %q is not KEY=value", hostImageDir, line)
		}
		if strings.HasSuffix(key, "_VERSION") {
			args = append(args, dagger.BuildArg{Name: key, Value: value})
		}
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("%s/versions.env pins no *_VERSION", hostImageDir)
	}
	return args, nil
}

// MirrorHostImageBase copies the Host Image's bootc base, as source is
// now, to repository, because quay.io deletes an old digest of
// fedora-bootc within days of a rebuild. crane copies the
// multi-architecture index and every image in it byte for byte, so the
// mirror has the source's digest. The copy's tag is the source tag, the
// date and the first 8 hex digits of the source digest, for example
// 44-20260929-f59997f5. It returns the source and the mirror by digest,
// and the make command that pins the mirror in hostimage/Containerfile.
// Only a person runs it, through `dagger call` or the manual workflow
// mirror-host-image-base.yml (hostimage/README.md).
//
// +cache="never"
func (m *BatteryOperator) MirrorHostImageBase(
	ctx context.Context,
	// The registry user.
	username string,
	// The registry password or token.
	password *dagger.Secret,
	// The base image to copy, by tag.
	// +default="quay.io/fedora/fedora-bootc:44"
	source string,
	// The repository of the mirror.
	// +default="ghcr.io/phoban01/battery-operator/fedora-bootc"
	repository string,
) (string, error) {
	sourceRepo, sourceTag, err := splitTag(source)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	registry, _, _ := strings.Cut(repository, "/")
	crane := dag.Container().
		From(craneImage).
		// A tag moves, so no step may come from the cache.
		WithEnvVariable("MIRRORED_AT", now.Format(time.RFC3339Nano)).
		WithEnvVariable("REGISTRY", registry).
		WithEnvVariable("REGISTRY_USER", username).
		WithSecretVariable("REGISTRY_PASSWORD", password).
		WithExec([]string{"sh", "-ec",
			`printf '%s' "$REGISTRY_PASSWORD" | crane auth login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin`})

	digest, err := craneDigest(ctx, crane, source)
	if err != nil {
		return "", err
	}
	tag := fmt.Sprintf("%s-%s-%s", sourceTag, now.Format("20060102"), strings.TrimPrefix(digest, "sha256:")[:8])
	pinned := sourceRepo + "@" + digest
	crane = crane.WithExec([]string{"crane", "copy", pinned, repository + ":" + tag})

	mirrored, err := craneDigest(ctx, crane, repository+":"+tag)
	if err != nil {
		return "", err
	}
	if mirrored != digest {
		return "", fmt.Errorf("the mirror %s:%s has digest %s, not the source's %s", repository, tag, mirrored, digest)
	}
	mirror := repository + ":" + tag + "@" + digest
	return fmt.Sprintf("source  %s@%s\nmirror  %s\n\nPin the mirror in hostimage/Containerfile with:\n  make host-image-base-pin MIRROR=%s SOURCE=%s@%s\n",
		source, digest, mirror, mirror, source, digest), nil
}

// craneDigest is the digest of ref: of its index, when it has one.
func craneDigest(ctx context.Context, crane *dagger.Container, ref string) (string, error) {
	out, err := crane.WithExec([]string{"crane", "digest", ref}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the digest of %s: %w", ref, err)
	}
	digest := strings.TrimSpace(out)
	if !digestRE.MatchString(digest) {
		return "", fmt.Errorf("the digest of %s is %q, not sha256:<64 hex digits>", ref, digest)
	}
	return digest, nil
}

// splitTag splits an image reference into its repository and its tag. A
// port in the registry's host name is not a tag.
func splitTag(ref string) (repo, tag string, err error) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref, "@") || strings.Contains(ref[i+1:], "/") {
		return "", "", fmt.Errorf("%q is not <repository>:<tag>", ref)
	}
	return ref[:i], ref[i+1:], nil
}
