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

package execagent

// Prefix is the prefix of every annotation the Exec Agent writes on its
// Host's Node, and all that config/exec-agent/admission-policy.yaml lets it
// change there (EA-051).
const Prefix = "battery.liquidmetal-x.dev/"

// The Node report (EA-034, EA-035), which the Inventory Controller and the Claim
// Controller read, and the Exec Agent's own bookkeeping of a drain (EA-040).
const (
	// AnnotationReady is "true" while the Exec Agent reports its Host ready
	// and "false" otherwise.
	AnnotationReady = Prefix + "exec-agent-ready"
	// AnnotationReason is the reason of the last readiness check, one word
	// in the manner of a condition's reason.
	AnnotationReason = Prefix + "exec-agent-reason"
	// AnnotationMessage says in words what the last readiness check found.
	AnnotationMessage = Prefix + "exec-agent-message"
	// AnnotationAddress is the `address:port` the Exec Agent serves its exec
	// API on (EA-002).
	AnnotationAddress = Prefix + "exec-agent-address"
	// AnnotationFlintlockdAddress is the `address:port` at which battery
	// reaches this Host's flintlockd (EA-035), which the Inventory
	// Controller gives battery (IN-003).
	AnnotationFlintlockdAddress = Prefix + "flintlockd-address"
	// AnnotationDrainStarted is when the Exec Agent first saw its Host's
	// Node unschedulable, so that a restart does not restart the drain
	// timeout (EA-040).
	AnnotationDrainStarted = Prefix + "drain-started"
)

// Reasons of the readiness the agent reports in AnnotationReason.
const (
	ReasonReady              = "Ready"
	ReasonHostImageNotReady  = "HostImageNotReady"
	ReasonKVMUnavailable     = "KVMUnavailable"
	ReasonThinPoolMissing    = "ThinPoolMissing"
	ReasonFlintlockdNotReady = "FlintlockdNotReady"
	ReasonExecDisabled       = "ExecDisabled"
)
