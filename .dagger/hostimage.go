package main

// The Host Image (hostimage/, docs/requirements/11-host-image.md): a bootc
// image built from its own Containerfile, for linux/amd64 and linux/arm64
// (HI-002, docs/adr/0007-reference-host-image-and-cluster-api.md).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"dagger/battery-operator/internal/dagger"
)

// craneImage copies the Host Image's base to its mirror
// (MirrorHostImageBase), and joins the Host Image of each platform into one
// index (HostImageIndex). crane copies an index and its images unchanged.
// The debug variant has a shell, which the registry login needs.
const craneImage = "gcr.io/go-containerregistry/crane/debug:v0.22.1@sha256:e78770b31258a3846f878036d9c1f63fbe4c871f9f56990bf77fd95c013e3c1b"

// digestRE matches a sha256 digest.
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// HostImage builds the Host Image from hostimage/Containerfile for
// platform, linux/amd64 or linux/arm64. The build runs the image's check
// stage, and fails when a check fails: the image takes a file that only the
// check stage writes. Every *_VERSION of hostimage/versions.env is passed as
// a build argument, which the Containerfile turns into an OCI label.
func (m *BatteryOperator) HostImage(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. The default is the engine's:
	// a build for another platform runs under emulation, which is slow.
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	platform, err := hostImagePlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
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
		Platform:   platform,
		BuildArgs:  args,
	}), nil
}

// hostImagePlatform is platform, or the engine's platform when it is empty,
// and fails unless it is one of hostImagePlatforms.
func hostImagePlatform(ctx context.Context, platform dagger.Platform) (dagger.Platform, error) {
	if platform == "" {
		var err error
		if platform, err = dag.DefaultPlatform(ctx); err != nil {
			return "", err
		}
	}
	if !slices.Contains(hostImagePlatforms, platform) {
		return "", fmt.Errorf("the Host Image is built for %v, not %s", hostImagePlatforms, platform)
	}
	return platform, nil
}

// HostImageCheck builds the Host Image for platform, runs its check stage
// again inside the built image, and then the half of the checks that
// cannot run inside it, hostimage/check-labels.sh: that the image's OCI
// labels are the versions of hostimage/versions.env, that its platform is
// the one it was built for, and that the versions file in it is that file.
func (m *BatteryOperator) HostImageCheck(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. The default is the engine's.
	// +optional
	platform dagger.Platform,
) (string, error) {
	platform, err := hostImagePlatform(ctx, platform)
	if err != nil {
		return "", err
	}
	img, err := m.HostImage(ctx, platform)
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
	got, err := img.Platform(ctx)
	if err != nil {
		return checks, fmt.Errorf("reading the Host Image's platform: %w", err)
	}
	facts := dag.Directory().
		WithNewFile("labels", strings.Join(lines, "\n")+"\n").
		WithNewFile("platform", string(got)+"\n").
		WithNewFile("want-platform", string(platform)+"\n").
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

// HostImagePublish builds the Host Image for each of platforms, its check
// stage included, and pushes it at every tag to repository. For more than
// one platform it pushes one multi-architecture index; for one, that
// platform's image alone. It returns the references it pushed, with their
// digests.
//
// Building both platforms on one machine emulates one of them, which is
// slow. So CI builds each platform on a runner of its own architecture and
// pushes it alone, at the commit SHA and the platform's architecture, and
// HostImageIndex then joins the two into the index a version tag
// publishes.
func (m *BatteryOperator) HostImagePublish(
	ctx context.Context,
	// The tags, for example a commit SHA or v0.1.0.
	tags []string,
	// The repository.
	// +default="ghcr.io/phoban01/battery-operator/host-image"
	repository string,
	// The platforms. The default is linux/amd64 and linux/arm64.
	// +optional
	platforms []dagger.Platform,
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
	if len(platforms) == 0 {
		platforms = hostImagePlatforms
	}
	variants := make([]*dagger.Container, 0, len(platforms))
	for _, p := range platforms {
		if !slices.Contains(hostImagePlatforms, p) {
			return "", fmt.Errorf("the Host Image is built for %v, not %s", hostImagePlatforms, p)
		}
		img, err := m.HostImage(ctx, p)
		if err != nil {
			return "", err
		}
		variants = append(variants, img)
	}
	// One platform is pushed as an image rather than an index of one, so
	// that HostImageIndex can put it in an index.
	ctr := variants[0]
	var opts dagger.ContainerPublishOpts
	if len(variants) > 1 {
		ctr = dag.Container()
		opts.PlatformVariants = variants
	}
	if username != "" {
		registry, _, _ := strings.Cut(repository, "/")
		ctr = ctr.WithRegistryAuth(registry, username, password)
	}
	var out strings.Builder
	for _, tag := range tags {
		ref, err := ctr.Publish(ctx, repository+":"+tag, opts)
		if err != nil {
			return out.String(), fmt.Errorf("publishing the Host Image: %w", err)
		}
		fmt.Fprintln(&out, ref)
	}
	return out.String(), nil
}

// HostImageIndex joins the Host Image of each platform, which
// HostImagePublish pushed alone to repository at <from>-<arch> (for
// example 3f2c...-arm64), into one multi-architecture index, and pushes the
// index at every tag. It checks that each source is a single image of its
// platform, and that the index it pushed holds exactly linux/amd64 and
// linux/arm64. It returns the references it pushed, with their digest.
//
// +cache="never"
func (m *BatteryOperator) HostImageIndex(
	ctx context.Context,
	// The tags of the index, for example a commit SHA and v0.1.0.
	tags []string,
	// The tag prefix of the images of each platform, for example the commit
	// SHA.
	from string,
	// The registry user.
	username string,
	// The registry password or token.
	password *dagger.Secret,
	// The repository.
	// +default="ghcr.io/phoban01/battery-operator/host-image"
	repository string,
) (string, error) {
	if len(tags) == 0 {
		return "", fmt.Errorf("no tags given")
	}
	crane := craneLogin(repository, username, password)

	args := []string{"crane", "index", "append"}
	for _, p := range hostImagePlatforms {
		_, arch, _ := strings.Cut(string(p), "/")
		ref := repository + ":" + from + "-" + arch
		digest, err := craneDigest(ctx, crane, ref)
		if err != nil {
			return "", err
		}
		config, err := crane.WithExec([]string{"crane", "config", repository + "@" + digest}).Stdout(ctx)
		if err != nil {
			return "", fmt.Errorf("reading the image config of %s: %w", ref, err)
		}
		var c struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		}
		if err := json.Unmarshal([]byte(config), &c); err != nil {
			return "", fmt.Errorf("the image config of %s: %w", ref, err)
		}
		if got := dagger.Platform(c.OS + "/" + c.Architecture); got != p {
			return "", fmt.Errorf("%s is %s, not %s", ref, got, p)
		}
		args = append(args, "-m", repository+"@"+digest)
	}
	first := repository + ":" + tags[0]
	crane = crane.WithExec(append(args, "-t", first))
	for _, tag := range tags[1:] {
		crane = crane.WithExec([]string{"crane", "tag", first, tag})
	}

	digest, err := craneDigest(ctx, crane, first)
	if err != nil {
		return "", err
	}
	got, err := craneIndexPlatforms(ctx, crane, repository+"@"+digest)
	if err != nil {
		return "", err
	}
	if !slices.Equal(got, hostImagePlatforms) {
		return "", fmt.Errorf("the index %s@%s holds %v, not %v", repository, digest, got, hostImagePlatforms)
	}
	var out strings.Builder
	for _, tag := range tags {
		fmt.Fprintf(&out, "%s:%s@%s\n", repository, tag, digest)
	}
	return out.String(), nil
}

// craneLogin is crane, logged in to the registry of repository. Every step
// after it runs anew: a tag moves, so none may come from the cache.
func craneLogin(repository, username string, password *dagger.Secret) *dagger.Container {
	registry, _, _ := strings.Cut(repository, "/")
	return dag.Container().
		From(craneImage).
		WithEnvVariable("CRANE_AT", time.Now().UTC().Format(time.RFC3339Nano)).
		WithEnvVariable("REGISTRY", registry).
		WithEnvVariable("REGISTRY_USER", username).
		WithSecretVariable("REGISTRY_PASSWORD", password).
		WithExec([]string{"sh", "-ec",
			`printf '%s' "$REGISTRY_PASSWORD" | crane auth login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin`})
}

// craneIndexPlatforms is the sorted os/architecture of every image in the
// index ref. It fails when ref is not an index.
func craneIndexPlatforms(ctx context.Context, crane *dagger.Container, ref string) ([]dagger.Platform, error) {
	out, err := crane.WithExec([]string{"crane", "manifest", ref}).Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the manifest of %s: %w", ref, err)
	}
	var index struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform *struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal([]byte(out), &index); err != nil {
		return nil, fmt.Errorf("the manifest of %s: %w", ref, err)
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("%s is not a multi-architecture index (%s)", ref, index.MediaType)
	}
	var got []dagger.Platform
	for _, d := range index.Manifests {
		if d.Platform == nil {
			return nil, fmt.Errorf("the index %s has an entry without a platform", ref)
		}
		got = append(got, dagger.Platform(d.Platform.OS+"/"+d.Platform.Architecture))
	}
	slices.Sort(got)
	return got, nil
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
	crane := craneLogin(repository, username, password)

	digest, err := craneDigest(ctx, crane, source)
	if err != nil {
		return "", err
	}
	// The Containerfile pins this digest for every platform of the Host
	// Image (HI-002), so it has to be an index that holds each of them.
	held, err := craneIndexPlatforms(ctx, crane, sourceRepo+"@"+digest)
	if err != nil {
		return "", err
	}
	for _, p := range hostImagePlatforms {
		if !slices.Contains(held, p) {
			return "", fmt.Errorf("%s@%s holds %v, not the Host Image's %s", source, digest, held, p)
		}
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
