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
// where the Manifests put battery's configuration file and its client
// certificate, and the configuration file they ship.
//
// battery (poolmgrd) reads one JSON file at startup, with the schema of
// its internal/config package, mirrored here as File. Since battery v0.5.0
// the file names no Hosts, and battery refuses one that does (BA-087):
// battery keeps its Hosts in its database, and the Inventory Controller
// keeps them in step through battery's HostAdmin API (ADR 0008). The
// Manifests ship the file in a ConfigMap (DP-036) that only battery's
// container mounts; nothing in the Operator writes it. battery's gRPC API
// and its metrics listen on loopback only (DP-001).
//
// Every Host is reached over mutual TLS with battery's client certificate,
// verified against the serving CA (DP-005), at the paths the Manifests
// mount them on. battery reads those files when it connects to a Host
// (BA-081). The Operator's container mounts the same Secret volume, so
// that the Inventory Controller can tell when a renewed certificate has
// reached battery's files and make battery read them again (DP-035).
//
// Up to battery v0.4.0 this package also rendered battery's Hosts into the
// file and restarted battery through a shared process namespace (DP-006,
// withdrawn).
package batterysidecar
