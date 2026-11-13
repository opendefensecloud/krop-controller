// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// Ready condition reasons, one per way a pass can end short of Ready (#49). The
// consumer reads them to tell a slow instance from one that needs a fix, so
// passes the consumer would act on alike share a reason.
const (
	// reasonInProgress: nothing to do but wait, a node or its data is not ready.
	reasonInProgress = "InProgress"
	// reasonReferenceNotFound: an external ref's object does not exist.
	reasonReferenceNotFound = "ReferenceNotFound"
	// reasonEvaluationFailed: an includeWhen or template expression failed.
	reasonEvaluationFailed = "EvaluationFailed"
	// reasonReferenceFailed: reading an external ref failed (e.g. forbidden).
	reasonReferenceFailed = "ReferenceFailed"
	// reasonApplyFailed: the target refused a child (admission, conflict, ...).
	reasonApplyFailed = "ApplyFailed"
	// reasonRoutingFailed: no applier or reader for the node's target.
	reasonRoutingFailed = "RoutingFailed"
)

// Values of status.state, kro's vocabulary so the STATE column reads the same as
// on a kro instance.
const (
	stateActive     = "ACTIVE"
	stateInProgress = "IN_PROGRESS"
	stateError      = "ERROR"
)

// maxConditionMessage caps a condition message. An apply error can quote a whole
// admission response; the first part names the cause.
const maxConditionMessage = 1024

// readyReport is what the Ready condition says about a pass that is not ready:
// its reason and its message.
type readyReport struct {
	reason, message string
}

// applyResultConditions updates the Ready and Progressing conditions in place
// from the engine Result. Ready carries rep, why the instance is not ready;
// Progressing carries how far the pass got, as a percentage only: node names and
// counts would expose how the provider built the offering.
func applyResultConditions(conditions *[]metav1.Condition, res kropengine.Result, rep readyReport, generation int64) {
	ready := metav1.Condition{Type: conditionReady, ObservedGeneration: generation}
	progressing := metav1.Condition{Type: conditionProgressing, ObservedGeneration: generation}
	if res.Ready {
		ready.Status = metav1.ConditionTrue
		ready.Reason = reasonReady
		ready.Message = "all resources are ready"
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = reasonReconciled
		progressing.Message = "100%"
	} else {
		ready.Status = metav1.ConditionFalse
		ready.Reason = rep.reason
		ready.Message = truncate(rep.message)
		progressing.Status = metav1.ConditionTrue
		progressing.Reason = rep.reason
		progressing.Message = fmt.Sprintf("%d%%", percentReady(res))
	}
	meta.SetStatusCondition(conditions, ready)
	meta.SetStatusCondition(conditions, progressing)
}

// report describes a not-ready pass for the consumer. err is the engine's error,
// nil unless the pass failed.
//
// Text the consumer could see anyway is passed through: errors from their own
// workspace (consumer target) and the Ready message of a consumer child. Text
// from the provider workspace or the host cluster is not: it can name host
// policies, projects or objects. Those, and evaluation errors (which quote the
// blueprint's expressions), get a fixed message naming the node, the class of
// failure and a reference: the instance UID, which the reconcile error, every
// krop log line of the instance and every child's instance-uid label carry, so
// the provider finds the raw error and the child from it (#49).
func (r *Reconciler) report(res kropengine.Result, err error, instanceUID string) readyReport {
	ref := ", ref " + instanceUID
	consumer := func(node string) bool { return kropengine.TargetForNode(node, r.Routing) == kropengine.TargetConsumer }

	switch res.Outcome {
	case kropengine.OutcomeNotReady:
		return readyReport{reasonInProgress, notReadyMessage(res.Node, res, consumer(res.Node), ref)}
	case kropengine.OutcomePending:
		// A node pending on a not-ready dependency: the dependency is the cause
		// the consumer can act on, so Ready names it.
		if res.Dependency != "" {
			return readyReport{reasonInProgress, notReadyMessage(res.Dependency, res, consumer(res.Dependency), ref)}
		}

		return readyReport{reasonInProgress, res.Node + ": waiting for data from other resources"}
	case kropengine.OutcomeExternalNotFound:
		if consumer(res.Node) {
			return readyReport{reasonReferenceNotFound, res.Node + ": " + res.Reason}
		}

		return readyReport{reasonReferenceNotFound, res.Node + ": the referenced object does not exist" + ref}
	case kropengine.OutcomeFailed:
		switch res.Stage {
		case kropengine.StageEvaluate:
			msg := res.Node + ": cannot evaluate the blueprint"
			if fields := specFields(res.Reason); len(fields) > 0 {
				msg += " with " + strings.Join(fields, ", ")
			}

			return readyReport{reasonEvaluationFailed, msg + ref}
		case kropengine.StageRead:
			if consumer(res.Node) {
				return readyReport{reasonReferenceFailed, res.Node + ": " + cause(err)}
			}

			return readyReport{reasonReferenceFailed, res.Node + ": the provider cannot read the referenced object" + statusReason(err) + ref}
		case kropengine.StageApply:
			if consumer(res.Node) {
				return readyReport{reasonApplyFailed, res.Node + ": " + cause(err)}
			}

			return readyReport{reasonApplyFailed, res.Node + ": the provider could not apply the resource" + statusReason(err) + ref}
		case kropengine.StageRoute:
			return readyReport{reasonRoutingFailed, res.Node + ": the provider has not configured this resource's target" + ref}
		}
	case kropengine.OutcomeComplete:
	}

	return readyReport{reasonInProgress, "not ready" + ref}
}

// notReadyMessage describes the not-ready node: its Ready condition's reason
// code, plus the condition's message only for a consumer child.
func notReadyMessage(node string, res kropengine.Result, consumer bool, ref string) string {
	if consumer && res.ReadyMessage != "" {
		if res.ReadyReason != "" {
			return node + ": " + res.ReadyReason + ": " + res.ReadyMessage
		}

		return node + ": " + res.ReadyMessage
	}
	msg := node + ": not ready"
	if res.ReadyReason != "" {
		msg += " (" + res.ReadyReason + ")"
	}
	if consumer {
		return msg
	}

	return msg + ref
}

// cause strips the engine's "node <id>: <stage> to <target>:" wrapping from err,
// leaving the API server's message.
func cause(err error) string {
	if u := errors.Unwrap(err); u != nil {
		return u.Error()
	}
	if err != nil {
		return err.Error()
	}

	return ""
}

// statusReason returns " (<StatusReason>)" for an API error, e.g. " (Forbidden)",
// a code that names the class of failure without its text.
func statusReason(err error) string {
	if reason := apierrors.ReasonForError(err); reason != metav1.StatusReasonUnknown {
		return " (" + string(reason) + ")"
	}

	return ""
}

// specFieldRef matches a reference to the instance spec in a kro error.
var specFieldRef = regexp.MustCompile(`schema\.(spec(?:\.[A-Za-z0-9_]+)+)`)

// specFields returns the spec fields a failed expression reads, in order and
// without duplicates: the consumer's own field names, without the expression.
func specFields(reason string) []string {
	var out []string
	for _, m := range specFieldRef.FindAllStringSubmatch(reason, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}

	return out
}

// percentReady is the share of ready nodes, rounded down so a not-ready instance
// never shows 100%. It is the whole Progressing message, which list views can
// show as a progress column.
func percentReady(res kropengine.Result) int {
	if res.Nodes == 0 {
		return 0
	}

	return res.ReadyNodes * 100 / res.Nodes
}

// instanceState maps the Result and the pass's error to status.state. ERROR means
// the instance will not recover by retrying: an expression that cannot evaluate,
// a node with no target, or a request the API server refuses as such. Any other
// failure (a conflict, a timeout, an unreachable or overloaded API server) clears
// on a later pass, so the instance stays IN_PROGRESS.
func instanceState(res kropengine.Result, err error) string {
	switch {
	case res.Ready:
		return stateActive
	case res.Outcome != kropengine.OutcomeFailed:
		return stateInProgress
	case res.Stage == kropengine.StageEvaluate, res.Stage == kropengine.StageRoute:
		return stateError
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err), apierrors.IsForbidden(err),
		apierrors.IsUnauthorized(err), apierrors.IsMethodNotSupported(err), apierrors.IsRequestEntityTooLargeError(err):
		return stateError
	}

	return stateInProgress
}

// truncate cuts s to maxConditionMessage bytes on a rune boundary.
func truncate(s string) string {
	if len(s) <= maxConditionMessage {
		return s
	}
	cut := maxConditionMessage - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + "..."
}
