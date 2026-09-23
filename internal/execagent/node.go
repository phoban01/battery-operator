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

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/battery-operator/internal/hostcheck"
)

// readiness is the outcome of one readiness check.
type readiness struct {
	ready   bool
	reason  string
	message string
}

// maxMessageLength caps the readiness message on the Node, which lists
// everything that is wrong and could otherwise grow without bound.
const maxMessageLength = 1024

//= docs/requirements/05-exec-agent.md#host-checks
//# The Exec Agent SHALL report its Host not ready while any file
//# in the not ready reason directory names a reason, and SHALL report that
//# reason.

//= docs/requirements/05-exec-agent.md#host-checks
//# The Exec Agent SHALL read not ready reasons from the directory
//# its configuration names.

// checkReadiness decides whether the Host is ready. Every check runs, so
// that the message lists everything that is wrong at once; the reason is
// that of the first failing check, in the order an operator would want to
// fix them: a Host Image unit that gave up (without KVM flintlockd is not
// even started), then flintlockd.
//
// The flintlockd check is flintlock-runner's, moved as it is: flintlockd
// has to answer ServerInfo with exec enabled. The rest of the prerequisite
// scan, and its citations, are #17's (EA-030 to EA-032).
func checkReadiness(ctx context.Context, cfg *Config, fl *Flintlockd) readiness {
	var problems []string
	reason := ""
	fail := func(why, message string) {
		if reason == "" {
			reason = why
		}
		problems = append(problems, message)
	}

	reasons, err := hostcheck.ReadNotReadyReasons(cfg.NotReadyDir)
	if err != nil {
		fail(ReasonHostImageNotReady, err.Error())
	}
	for _, r := range reasons {
		fail(ReasonHostImageNotReady, r.Unit+": "+r.Reason)
	}

	infoCtx, cancel := context.WithTimeout(ctx, cfg.CallTimeout)
	info, err := fl.vms.ServerInfo(infoCtx, &emptypb.Empty{})
	cancel()
	switch {
	case err != nil:
		fail(ReasonFlintlockdNotReady, "flintlockd does not answer ServerInfo: "+err.Error())
	case !info.GetExec().GetEnabled():
		fail(ReasonExecDisabled, "flintlockd reports the exec service disabled")
	}

	if reason != "" {
		message := strings.Join(problems, "; ")
		if len(message) > maxMessageLength {
			message = message[:maxMessageLength]
		}
		return readiness{reason: reason, message: message}
	}
	return readiness{ready: true, reason: ReasonReady, message: "flintlockd answers with exec enabled and the Host Image reports no reason to be not ready"}
}

//= docs/requirements/05-exec-agent.md#host-checks
//# The Exec Agent SHALL publish its Node report on its Host's Node
//# as the annotation `battery.liquidmetal-x.dev/exec-agent-ready` set to
//# `true` or `false`, with the reason and message of the last check in
//# `battery.liquidmetal-x.dev/exec-agent-reason` and
//# `battery.liquidmetal-x.dev/exec-agent-message`, and its own address in
//# `battery.liquidmetal-x.dev/exec-agent-address`.

// nodeReport is the Node report the agent keeps on its Host's Node: the
// Host's readiness, and the address of the exec API.
func nodeReport(r readiness, agentAddress string) map[string]any {
	return map[string]any{
		AnnotationReady:   strconv.FormatBool(r.ready),
		AnnotationReason:  r.reason,
		AnnotationMessage: r.message,
		AnnotationAddress: agentAddress,
	}
}

// annotator patches the agent's annotations onto its Host's Node, and only
// when they differ from what it last wrote, with a full write every resync
// period in case something else changed them.
type annotator struct {
	kube     kubernetes.Interface
	hostNode string
	resync   time.Duration
	last     map[string]any
	lastAt   time.Time
}

// publish writes want to the Host's Node when it has changed.
func (a *annotator) publish(ctx context.Context, want map[string]any, now time.Time) error {
	if a.last != nil && maps.Equal(a.last, want) && now.Sub(a.lastAt) < a.resync {
		return nil
	}
	if err := patchNodeAnnotations(ctx, a.kube, a.hostNode, want); err != nil {
		a.last = nil
		return err
	}
	a.last, a.lastAt = maps.Clone(want), now
	return nil
}

// patchNodeAnnotations applies a JSON merge patch of annotations, and of
// nothing else, to a Node: the admission policy of EA-051 refuses the agent
// any other change.
func patchNodeAnnotations(ctx context.Context, kube kubernetes.Interface, node string, annotations map[string]any) error {
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	if _, err := kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, data, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotating the host's node %s: %w", node, err)
	}
	return nil
}

// hostInternalIP is the Host's internal address.
func hostInternalIP(host *corev1.Node) string {
	for _, addr := range host.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}
