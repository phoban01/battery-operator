package main

// The Host Image (hostimage/, docs/requirements/11-host-image.md): a bootc
// image built from its own Containerfile, for linux/amd64 only
// (docs/adr/0007-reference-host-image-and-cluster-api.md).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dagger/battery-operator/internal/dagger"
)

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
