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

// Package batterysidecar is the Operator's side of battery running as a
// sidecar in its pod (docs/requirements/06-deployment.md#battery-sidecar):
// battery's configuration file, and restarting battery when that file
// changes.
//
// # Configuration
//
// battery (poolmgrd v0.3.3) reads one JSON file at startup, with the
// schema of its internal/config package, mirrored here as File. The
// Manifests ship it in a ConfigMap the Operator may update (DP-004), and
// the Inventory Controller renders a new one with Render whenever its
// Hosts change. Every Host is reached over mutual TLS with battery's client
// certificate, verified against the serving CA (DP-005), at the paths the
// Manifests mount them on. battery's gRPC API and its metrics listen on
// loopback only (DP-001).
//
// poolmgrd v0.3.3 refuses to start with no Hosts. An empty inventory
// therefore renders one placeholder Host, PlaceholderHostName, whose name
// is not a valid Node name and whose address, under the reserved .invalid
// top-level domain, never resolves. battery connects to a Host lazily and
// only for a Pool that names it, and the Operator never names the
// placeholder in a Pool, so battery starts, listens and never dials it.
//
// # Restarting battery (DP-006)
//
// poolmgrd v0.3.3 reads its configuration once. It handles SIGINT and
// SIGTERM by stopping gracefully and exiting, has no reload and does not
// handle SIGHUP (which, unhandled, kills a Go program without running its
// deferred cleanup, the SQLite store's Close among them). It does not watch
// its file either. It reads its flintlockd client certificate once too,
// when it starts, and presents that certificate until it exits (BA-061),
// so a certificate cert-manager has renewed reaches the Hosts only when
// battery restarts (DP-007).
//
// The mechanism chosen is a shared process namespace and SIGTERM: the
// Operator's pod sets shareProcessNamespace, both containers run as the
// same non-root user (65532, the distroless nonroot user of both images),
// so the Operator may signal battery without any capability, and the pod's
// restartPolicy, Always, has the kubelet start battery again with the new
// file. Restarter.Restart is the single hook: the Inventory Controller
// writes the ConfigMap and calls it with the content it wrote and the
// client certificate battery's Secret holds, whether it restarts battery
// for its Hosts, for a renewed certificate, or both. Restart
//
//  1. waits until the Operator's own mounts of the ConfigMap and of the
//     client certificate's Secret hold that content. The kubelet updates a
//     ConfigMap or Secret volume some time after the object changes, and
//     both containers mount the same volumes, so what the Operator reads is
//     what battery would read;
//  2. finds poolmgrd in the shared /proc and sends it SIGTERM;
//  3. waits until that process has gone, so that the old battery, still
//     draining, cannot answer for the new one;
//  4. waits until battery answers again.
//
// Gated, the battery client the controllers use, holds every call from
// just before the signal until Restart returns, so no controller calls
// battery between the signal and battery answering again.
//
// The kubelet restarts an exited container with a back-off that grows
// while the container keeps exiting within ten minutes of starting (10s,
// 20s, 40s, ... up to five minutes), so the Inventory Controller batches
// Host changes into restart windows (IN-012) rather than restarting
// battery for each one.
//
// The alternatives were rejected:
//
//   - battery exiting when its file changes needs a change to battery or a
//     wrapper in its image, and battery is not changed (ADR 0001);
//   - a pod template annotation with the file's hash restarts the whole
//     pod, the Operator with it, through the Deployment's Recreate
//     strategy;
//   - a liveness probe that fails on a stale file needs a shell or a probe
//     binary in battery's distroless image, which has neither.
//
// The shared process namespace lets each container see the other's
// processes, and, as the same user, read the other's /proc entries: the
// Operator can read battery's environment and files, and battery the
// Operator's. Both are the Operator's own code and configuration, in one
// pod that holds one identity, so this widens nothing across a trust
// boundary.
package batterysidecar
