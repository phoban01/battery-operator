package main

// The Guest Images (guestimage/, docs/requirements/13-guest-image.md): the
// kernel image and the root filesystem image a Pool's MicroVMs boot from,
// each built from its own Containerfile for linux/amd64 and linux/arm64
// (GI-001). Both are small. The kernel is a download, checked on the
// engine's platform, so it needs no emulation; the root filesystem runs apt
// on its own platform, under emulation on the other one. So one engine
// builds both platforms, and a version tag pushes one index of each from
// one job.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"dagger/battery-operator/internal/dagger"
)

const (
	// guestImageDir holds the Guest Images' sources, one build context per
	// image.
	guestImageDir = "guestimage"
	// guestImagePrefix is the repository below which GuestImagePublish
	// pushes guest-kernel and guest-rootfs by default (GI-030).
	guestImagePrefix = "ghcr.io/phoban01/battery-operator"
	// guestKernelVersionLabel and guestAgentVersionLabel are the labels
	// that record the versions the Containerfiles pin.
	guestKernelVersionLabel = "org.opencontainers.image.version"
	guestAgentVersionLabel  = "dev.liquidmetal-x.battery.guest-agent"
)

// guestImagePlatforms are what the Guest Images are built and published for
// (GI-001).
var guestImagePlatforms = []dagger.Platform{"linux/amd64", "linux/arm64"}

// guestImages are the two images: the name of each, which is its build
// context under guestimage/ and its repository's last part.
var guestImages = []struct{ dir, repository string }{
	{"kernel", "guest-kernel"},
	{"rootfs", "guest-rootfs"},
}

// GuestKernel builds the Guest Images' kernel image from
// guestimage/kernel/Containerfile for platform, linux/amd64 or linux/arm64.
// The build runs the image's check stage, and fails when a check fails.
func (m *BatteryOperator) GuestKernel(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. The default is the engine's.
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	return m.guestImage(ctx, "kernel", platform)
}

// GuestRootfs builds the Guest Images' root filesystem image from
// guestimage/rootfs/Containerfile for platform, linux/amd64 or
// linux/arm64. The build runs the image's check stage, and fails when a
// check fails. A platform other than the engine's runs under emulation.
func (m *BatteryOperator) GuestRootfs(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. The default is the engine's.
	// +optional
	platform dagger.Platform,
) (*dagger.Container, error) {
	return m.guestImage(ctx, "rootfs", platform)
}

// guestImage builds the image of guestimage/<dir> for platform.
func (m *BatteryOperator) guestImage(ctx context.Context, dir string, platform dagger.Platform) (*dagger.Container, error) {
	platform, err := guestImagePlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	img := m.Source.Directory(guestImageDir + "/" + dir).DockerBuild(dagger.DirectoryDockerBuildOpts{
		Dockerfile: "Containerfile",
		Platform:   platform,
	})
	got, err := img.Platform(ctx)
	if err != nil {
		return nil, err
	}
	if got == platform {
		return img, nil
	}
	// The engine gives an image whose last stage is FROM scratch the
	// engine's platform, whatever the build's. The files are the target
	// platform's, so put them, and the labels, in a container of that
	// platform.
	ctr := dag.Container(dagger.ContainerOpts{Platform: platform}).WithRootfs(img.Rootfs())
	labels, err := img.Labels(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		name, err := l.Name(ctx)
		if err != nil {
			return nil, err
		}
		value, err := l.Value(ctx)
		if err != nil {
			return nil, err
		}
		ctr = ctr.WithLabel(name, value)
	}
	return ctr, nil
}

// guestImagePlatform is platform, or the engine's platform when it is
// empty, and fails unless it is one of guestImagePlatforms.
func guestImagePlatform(ctx context.Context, platform dagger.Platform) (dagger.Platform, error) {
	if platform == "" {
		var err error
		if platform, err = dag.DefaultPlatform(ctx); err != nil {
			return "", err
		}
	}
	if !slices.Contains(guestImagePlatforms, platform) {
		return "", fmt.Errorf("the Guest Images are built for %v, not %s", guestImagePlatforms, platform)
	}
	return platform, nil
}

// GuestImageCheck builds both Guest Images for platform, check stages
// included, and runs each image's checks again on the image built: the
// kernel's guestimage/kernel/check.sh over the image's files, the root
// filesystem's guestimage/rootfs/check.sh inside it. It also checks that
// each image is of the platform it was built for. The versions the checks
// want come from the images' labels.
func (m *BatteryOperator) GuestImageCheck(
	ctx context.Context,
	// The platform, linux/amd64 or linux/arm64. The default is the engine's.
	// +optional
	platform dagger.Platform,
) (string, error) {
	platform, err := guestImagePlatform(ctx, platform)
	if err != nil {
		return "", err
	}
	_, arch, _ := strings.Cut(string(platform), "/")
	var out strings.Builder

	kernel, err := m.GuestKernel(ctx, platform)
	if err != nil {
		return "", err
	}
	if err := checkPlatform(ctx, "the kernel image", kernel, platform); err != nil {
		return "", err
	}
	version, err := kernel.Label(ctx, guestKernelVersionLabel)
	if err != nil || version == "" {
		return "", fmt.Errorf("the kernel image has no label %s: %v", guestKernelVersionLabel, err)
	}
	checks, err := dag.Container().
		From(goImage).
		WithDirectory("/image", kernel.Rootfs()).
		WithFile("/check.sh", m.Source.File(guestImageDir+"/kernel/check.sh")).
		WithExec([]string{"sh", "/check.sh", "/image", arch, version}).
		Stdout(ctx)
	out.WriteString("== the kernel image, linux/" + arch + "\n" + checks)
	if err != nil {
		return out.String(), fmt.Errorf("the kernel image's checks: %w", err)
	}

	rootfs, err := m.GuestRootfs(ctx, platform)
	if err != nil {
		return out.String(), err
	}
	if err := checkPlatform(ctx, "the root filesystem image", rootfs, platform); err != nil {
		return out.String(), err
	}
	agent, err := rootfs.Label(ctx, guestAgentVersionLabel)
	if err != nil || agent == "" {
		return out.String(), fmt.Errorf("the root filesystem image has no label %s: %v", guestAgentVersionLabel, err)
	}
	checks, err = rootfs.
		WithFile("/tmp/check.sh", m.Source.File(guestImageDir+"/rootfs/check.sh")).
		WithExec([]string{"bash", "/tmp/check.sh", agent}).
		Stdout(ctx)
	out.WriteString("== the root filesystem image, linux/" + arch + "\n" + checks)
	if err != nil {
		return out.String(), fmt.Errorf("the root filesystem image's checks: %w", err)
	}
	return out.String(), nil
}

// checkPlatform fails unless ctr's platform is want.
func checkPlatform(ctx context.Context, what string, ctr *dagger.Container, want dagger.Platform) error {
	got, err := ctr.Platform(ctx)
	if err != nil {
		return fmt.Errorf("reading the platform of %s: %w", what, err)
	}
	if got != want {
		return fmt.Errorf("%s is for %s, not %s", what, got, want)
	}
	return nil
}

// GuestImageLint lints the Guest Images' sources without building them,
// through guestimage/lint.sh: the base images' digest pins, the checksum of
// every download, and bash -n and shellcheck over the scripts.
func (m *BatteryOperator) GuestImageLint(ctx context.Context) (string, error) {
	return dag.Container().
		From(goImage).
		WithExec([]string{"sh", "-ec", `
apt-get update -qq
apt-get install -y -qq --no-install-recommends shellcheck >/dev/null
shellcheck --version | sed -n 2p
`}).
		WithDirectory(src, m.Source).
		WithWorkdir(src).
		WithEnvVariable("GUEST_IMAGE_REQUIRE_SHELLCHECK", "1").
		WithExec([]string{"guestimage/lint.sh"}).
		Stdout(ctx)
}

// guestImageLayout is the image of guestimage/<dir> for every platform of
// guestImagePlatforms, as an OCI image layout that holds one index of
// them, which guestimage/push.sh pushes.
func (m *BatteryOperator) guestImageLayout(ctx context.Context, dir string) (*dagger.Directory, error) {
	variants := make([]*dagger.Container, 0, len(guestImagePlatforms))
	for _, p := range guestImagePlatforms {
		img, err := m.guestImage(ctx, dir, p)
		if err != nil {
			return nil, err
		}
		variants = append(variants, img)
	}
	tarball := dag.Container().AsTarball(dagger.ContainerAsTarballOpts{
		PlatformVariants: variants,
		MediaTypes:       dagger.ImageMediaTypesOcimediaTypes,
	})
	return dag.Container().
		From(goImage).
		WithFile("/image.tar", tarball).
		WithExec([]string{"sh", "-ec", "mkdir /layout && tar -xf /image.tar -C /layout"}).
		Directory("/layout"), nil
}

// guestPushBase is the Go image with the source, crane, and the layout of
// each Guest Image at /layouts/<dir>.
func (m *BatteryOperator) guestPushBase(ctx context.Context) (*dagger.Container, error) {
	ctr := dag.Container().
		From(goImage).
		WithFile("/usr/local/bin/crane", dag.Container().From(craneImage).File("/ko-app/crane")).
		WithDirectory(src, m.Source).
		WithWorkdir(src)
	for _, g := range guestImages {
		layout, err := m.guestImageLayout(ctx, g.dir)
		if err != nil {
			return nil, err
		}
		ctr = ctr.WithDirectory("/layouts/"+g.dir, layout)
	}
	return ctr, nil
}

// GuestImagePublish builds both Guest Images for linux/amd64 and
// linux/arm64, check stages included, and pushes each as one
// multi-architecture index to <prefix>/guest-kernel and
// <prefix>/guest-rootfs at every tag, through guestimage/push.sh (GI-030).
// It returns each reference it pushed with its digest, one per line
// (GI-032).
//
// CI runs it on a version tag, with the version and the commit SHA:
//
//	dagger call guest-image-publish --tags=v0.4.0,$(git rev-parse HEAD) \
//	  --username=<user> --password=env://GITHUB_TOKEN
//
// +cache="never"
func (m *BatteryOperator) GuestImagePublish(
	ctx context.Context,
	// The tags, for example v0.4.0 and the commit SHA.
	tags []string,
	// The repository below which guest-kernel and guest-rootfs go.
	// +default="ghcr.io/phoban01/battery-operator"
	prefix string,
	// The registry user. Without one, the push uses no credentials.
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
	ctr, err := m.guestPushBase(ctx)
	if err != nil {
		return "", err
	}
	// A push always runs: a tag moves, so it may not come from the cache.
	ctr = ctr.WithEnvVariable("PUSHED_AT", time.Now().UTC().Format(time.RFC3339Nano))
	if username != "" {
		registry, _, _ := strings.Cut(prefix, "/")
		ctr = ctr.
			WithEnvVariable("REGISTRY", registry).
			WithEnvVariable("REGISTRY_USER", username).
			WithSecretVariable("REGISTRY_PASSWORD", password).
			WithExec([]string{"sh", "-ec",
				`printf '%s' "$REGISTRY_PASSWORD" | crane auth login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin`})
	}
	var out strings.Builder
	for _, g := range guestImages {
		pushed, err := ctr.
			WithExec(append([]string{"guestimage/push.sh", "/layouts/" + g.dir, prefix + "/" + g.repository}, tags...)).
			Stdout(ctx)
		out.WriteString(pushed)
		if err != nil {
			return out.String(), fmt.Errorf("publishing the %s image: %w", g.repository, err)
		}
	}
	return out.String(), nil
}

// GuestImagePublishCheck builds both Guest Images as GuestImagePublish
// does, and pushes them with guestimage/push.sh to a registry inside the
// check's own container, through guestimage/push-check.sh. That checks
// what push.sh printed and what the registry holds: one index per image,
// at both tags, for linux/amd64 and linux/arm64. It pushes nothing outside
// the container, so a pull request runs it.
func (m *BatteryOperator) GuestImagePublishCheck(ctx context.Context) (string, error) {
	ctr, err := m.guestPushBase(ctx)
	if err != nil {
		return "", err
	}
	return ctr.
		WithFile("/usr/local/bin/registry", dag.Container().From(registryImage).File("/bin/registry")).
		WithExec([]string{"sh", "-ec", "guestimage/push-check.sh /layouts/kernel /layouts/rootfs 2>&1"}).
		Stdout(ctx)
}
