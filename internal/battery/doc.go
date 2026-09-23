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

// Package battery is the Operator's client for battery's gRPC API
// (docs/requirements/06-deployment.md#battery-connection).
//
// battery runs as a sidecar in the Operator's pod and listens on loopback
// only (DP-001), so Dial refuses any other address. The connection is one
// long-lived gRPC ClientConn with keepalive and exponential reconnect
// backoff, and every unary call carries a deadline (DP-010). Each method
// maps one RPC of the poolmgr.v1alpha1 PoolAdmin, Lease and Events services
// and translates the status codes into the sentinel errors below (DP-011),
// so no caller imports gRPC codes. Connection.Check is the Operator's
// readiness check: it fails while battery does not answer (DP-012).
//
// The controllers depend on the Client interface, not on Connection, so
// that their tests can use a hand-written stub where the fake battery
// (package fakebattery) is too heavy. Messages are plain Go structs for the
// same reason; enums are aliases of the generated proto enums, so the
// client, the fake and the controllers share one definition.
//
// The connection, TLS, keepalive, deadline and status mapping come from
// flintlock-runner's internal/poolmgr client. Its policy layers (declaring
// Pools, tracking capacity, health) did not come along: they belong to the
// controllers here.
package battery
