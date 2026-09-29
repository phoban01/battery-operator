/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manifests_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// The media types and annotations of an artifact that `flux push artifact`
// makes, which Flux's OCIRepository pulls with no layerSelector.
const (
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	fluxConfigMediaType  = "application/vnd.cncf.flux.config.v1+json"
	fluxContentMediaType = "application/vnd.cncf.flux.content.v1.tar+gzip"
	sourceAnnotation     = "org.opencontainers.image.source"
	revisionAnnotation   = "org.opencontainers.image.revision"
	repositoryURL        = "https://github.com/phoban01/battery-operator"
)

// A release's images as the check renders them: pinned by tag and digest.
const (
	pinnedOperatorImage  = "ghcr.io/phoban01/battery-operator:v0.0.0-check@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	pinnedExecAgentImage = "ghcr.io/phoban01/battery-operator/exec-agent:v0.0.0-check@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// renderRelease runs hack/manifests-artifact.sh render, as `make
// build-installer` does, with the given images and battery's image at the
// version go.mod pins, and returns the directory it wrote.
func renderRelease(t *testing.T, operatorImage, execAgentImage string) string {
	t.Helper()
	root := moduleRoot(t)
	kustomize := os.Getenv("KUSTOMIZE")
	if kustomize == "" {
		kustomize = filepath.Join(root, "bin", "kustomize")
	}
	// Read config/ here, so that a change to it runs the test again (build
	// has the reason).
	build(t, "config/release")
	dir := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "hack", "manifests-artifact.sh"), "render", dir)
	cmd.Env = append(os.Environ(),
		"KUSTOMIZE="+kustomize,
		"IMG="+operatorImage,
		"EXEC_AGENT_IMG="+execAgentImage,
		"POOLMGRD_IMG="+batteryImage(t),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hack/manifests-artifact.sh render: %v\n%s", err, out)
	}
	return dir
}

// key names an object by kind, namespace and name.
func key(o object) string {
	return o.Kind + " " + o.Namespace + "/" + o.Name
}

// checkInstall checks a directory of the Manifests' artifact: that it holds
// install.yaml alone, that install.yaml is every object of config/default and
// config/exec-agent once, and that every container runs the image given for
// it, or battery's at the version go.mod pins.
func checkInstall(t *testing.T, dir, operatorImage, execAgentImage string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"install.yaml"}) {
		t.Fatalf("the artifact holds %v; want install.yaml alone", names)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	objs := parse(t, "install.yaml", rendered)

	got := make([]string, 0, len(objs))
	for _, o := range objs {
		got = append(got, key(o))
	}
	shipped := build(t, "config/default", "config/exec-agent")
	want := make([]string, 0, len(shipped))
	for _, o := range shipped {
		want = append(want, key(o))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("install.yaml holds\n  %s\nwant config/default and config/exec-agent:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// Deployments and DaemonSets are the only workloads, so their pod
	// templates name every image.
	for _, o := range objs {
		switch o.Kind {
		case "Pod", "ReplicaSet", "StatefulSet", "Job", "CronJob":
			t.Errorf("install.yaml has a %s, %s, whose images this test does not check", o.Kind, o.Name)
		}
	}
	images := map[string]string{}
	pod := func(owner string, spec corev1.PodSpec) {
		for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
			images[owner+"/"+c.Name] = c.Image
		}
	}
	for _, d := range all[appsv1.Deployment](t, objs, "Deployment") {
		pod("Deployment "+d.Name, d.Spec.Template.Spec)
	}
	for _, d := range all[appsv1.DaemonSet](t, objs, "DaemonSet") {
		pod("DaemonSet "+d.Name, d.Spec.Template.Spec)
	}
	wantImages := map[string]string{
		"Deployment " + operatorDeployment + "/manager":    operatorImage,
		"Deployment " + operatorDeployment + "/battery":    batteryImage(t),
		"DaemonSet battery-operator-exec-agent/exec-agent": execAgentImage,
	}
	for c, img := range images {
		if w, ok := wantImages[c]; !ok {
			t.Errorf("container %s runs %s, which the release does not pin", c, img)
		} else if img != w {
			t.Errorf("container %s runs %s; want %s", c, img, w)
		}
	}
	for c := range wantImages {
		if _, ok := images[c]; !ok {
			t.Errorf("install.yaml has no container %s", c)
		}
	}
}

//= docs/requirements/06-deployment.md#manifests-artifact
//= type=test
//# The artifact of DP-030 SHALL hold one file, `install.yaml`,
//# which is `config/release` rendered by kustomize: `config/default` and
//# `config/exec-agent` together.
//
//= docs/requirements/06-deployment.md#manifests-artifact
//= type=test
//# In the artifact of DP-030, the Manifests SHALL name the
//# Operator's and the Exec Agent's images by the version's tag and the digest
//# published for it, and battery's image by the battery version that
//# `go.mod` requires.

// TestReleaseRender renders the Manifests as a release does, and checks
// what the artifact would hold. TestManifestsArtifact checks the artifact
// itself.
func TestReleaseRender(t *testing.T) {
	dir := renderRelease(t, pinnedOperatorImage, pinnedExecAgentImage)
	checkInstall(t, dir, pinnedOperatorImage, pinnedExecAgentImage)
}

// ociManifest is the part of an OCI image manifest the artifact is checked
// by.
type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// sha256Digest is data's digest as a registry gives it.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

//= docs/requirements/06-deployment.md#manifests-artifact
//= type=test
//# When a version tag is pushed, the Manifests SHALL be published
//# as an OCI artifact at `ghcr.io/phoban01/battery-operator/manifests`,
//# tagged with the version and with the commit's full SHA.
//
//= docs/requirements/06-deployment.md#manifests-artifact
//= type=test
//# The artifact of DP-030 SHALL have a config of media type
//# `application/vnd.cncf.flux.config.v1+json` and a single layer of media
//# type `application/vnd.cncf.flux.content.v1.tar+gzip`, the media types that
//# `flux push artifact` gives.
//
//= docs/requirements/06-deployment.md#manifests-artifact
//= type=test
//# The artifact of DP-030 SHALL carry the annotation
//# `org.opencontainers.image.source`, the repository's URL, and the
//# annotation `org.opencontainers.image.revision`, the version and the
//# commit's full SHA as `<version>@sha1:<commit>`.

// TestManifestsArtifact checks an artifact that hack/manifests-artifact.sh
// pushed to a registry, as hack/manifests-artifact-check.sh fetched it:
// its manifest by the version's tag and by the commit's, what `flux pull
// artifact` extracted, and what was pushed. `dagger call manifests-check`
// runs it that way, in CI's manifests job; `make test` skips it.
//
// The check pushes to a registry of its own, not to ghcr.io, so the
// repository is given; hack/manifests-artifact.sh pushes to whichever it
// is given, and CI gives it ghcr.io/phoban01/battery-operator/manifests on a
// version tag.
func TestManifestsArtifact(t *testing.T) {
	art := os.Getenv("MANIFESTS_ARTIFACT")
	if art == "" {
		t.Skip("MANIFESTS_ARTIFACT is not set; hack/manifests-artifact-check.sh sets it")
	}
	repository := os.Getenv("MANIFESTS_ARTIFACT_REPOSITORY")
	version := os.Getenv("MANIFESTS_ARTIFACT_VERSION")
	commit := os.Getenv("MANIFESTS_ARTIFACT_COMMIT")
	operatorImage, execAgentImage := os.Getenv("IMG"), os.Getenv("EXEC_AGENT_IMG")
	if repository == "" || version == "" || commit == "" || operatorImage == "" || execAgentImage == "" {
		t.Fatal("set MANIFESTS_ARTIFACT_REPOSITORY, MANIFESTS_ARTIFACT_VERSION, MANIFESTS_ARTIFACT_COMMIT, IMG and EXEC_AGENT_IMG")
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(art, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// One artifact, at both tags, and the reference push printed.
	raw := read("manifest.json")
	digest := sha256Digest(raw)
	if d := sha256Digest(read("manifest-commit.json")); d != digest {
		t.Errorf("the artifact tagged %s is %s; the one tagged %s is %s", commit, d, version, digest)
	}
	pushed := strings.TrimSpace(string(read("pushed")))
	if want := repository + ":" + version + "@" + digest; pushed != want {
		t.Errorf("push printed %q; want %q", pushed, want)
	}

	var m ociManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if m.SchemaVersion != 2 || m.MediaType != ociManifestMediaType {
		t.Errorf("the manifest is schema %d, %s; want 2, %s", m.SchemaVersion, m.MediaType, ociManifestMediaType)
	}
	if m.Config.MediaType != fluxConfigMediaType {
		t.Errorf("the config's media type is %s; want %s", m.Config.MediaType, fluxConfigMediaType)
	}
	if len(m.Layers) != 1 {
		t.Fatalf("the artifact has %d layers; want 1", len(m.Layers))
	}
	if m.Layers[0].MediaType != fluxContentMediaType {
		t.Errorf("the layer's media type is %s; want %s", m.Layers[0].MediaType, fluxContentMediaType)
	}
	for name, want := range map[string]string{
		sourceAnnotation:   repositoryURL,
		revisionAnnotation: version + "@sha1:" + commit,
	} {
		if got := m.Annotations[name]; got != want {
			t.Errorf("annotation %s is %q; want %q", name, got, want)
		}
	}

	// What Flux extracts is what was rendered, and what a release holds.
	content := filepath.Join(art, "content")
	checkInstall(t, content, operatorImage, execAgentImage)
	if !bytes.Equal(read("content/install.yaml"), read("rendered.yaml")) {
		t.Error("the pulled install.yaml differs from the rendered one")
	}
}
