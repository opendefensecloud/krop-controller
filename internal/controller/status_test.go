// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// TestInstanceState verifies that ERROR means the instance will not recover on
// its own: a failure that a retry can clear (conflict, timeout, an unavailable
// API server) keeps the instance IN_PROGRESS.
func TestInstanceState(t *testing.T) {
	gr := schema.GroupResource{Resource: "configmaps"}
	failed := func(stage kropengine.Stage) kropengine.Result {
		return kropengine.Result{Outcome: kropengine.OutcomeFailed, Stage: stage, Node: "credentials"}
	}
	// The engine wraps the API error with the node and target.
	wrap := func(err error) error { return fmt.Errorf("node credentials: apply to consumer: %w", err) }

	cases := []struct {
		name string
		res  kropengine.Result
		err  error
		want string
	}{
		{"ready", kropengine.Result{Ready: true, Outcome: kropengine.OutcomeComplete}, nil, stateActive},
		{"child not ready", kropengine.Result{Outcome: kropengine.OutcomeNotReady}, nil, stateInProgress},
		{"reference missing", kropengine.Result{Outcome: kropengine.OutcomeExternalNotFound}, nil, stateInProgress},
		{"evaluation failed", failed(kropengine.StageEvaluate), errors.New("eval"), stateError},
		{"no target configured", failed(kropengine.StageRoute), errors.New("route"), stateError},
		{"apply denied by admission", failed(kropengine.StageApply), wrap(apierrors.NewBadRequest("denied")), stateError},
		{"apply invalid", failed(kropengine.StageApply), wrap(apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "x", nil)), stateError},
		{"apply forbidden", failed(kropengine.StageApply), wrap(apierrors.NewForbidden(gr, "x", errors.New("no"))), stateError},
		{"read forbidden", failed(kropengine.StageRead), wrap(apierrors.NewForbidden(gr, "x", errors.New("claim"))), stateError},
		{"apply conflict", failed(kropengine.StageApply), wrap(apierrors.NewConflict(gr, "x", errors.New("modified"))), stateInProgress},
		{"apply timeout", failed(kropengine.StageApply), wrap(apierrors.NewServerTimeout(gr, "apply", 1)), stateInProgress},
		{"apply unavailable", failed(kropengine.StageApply), wrap(apierrors.NewServiceUnavailable("down")), stateInProgress},
		{"apply too many requests", failed(kropengine.StageApply), wrap(apierrors.NewTooManyRequests("slow down", 1)), stateInProgress},
		{"apply internal error", failed(kropengine.StageApply), wrap(apierrors.NewInternalError(errors.New("boom"))), stateInProgress},
		{"apply connection refused", failed(kropengine.StageApply), wrap(errors.New("dial tcp: connection refused")), stateInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instanceState(tc.res, tc.err); got != tc.want {
				t.Fatalf("instanceState = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReport_WaitingIsOneReason verifies that every "wait, nothing to do" pass
// reports the same Ready reason: a child not ready, a node pending on a not-ready
// dependency, and a node pending on data from other nodes.
func TestReport_WaitingIsOneReason(t *testing.T) {
	r := &Reconciler{}
	for _, res := range []kropengine.Result{
		{Outcome: kropengine.OutcomeNotReady, Node: "bucket"},
		{Outcome: kropengine.OutcomePending, Node: "credentials", Dependency: "bucket"},
		{Outcome: kropengine.OutcomePending, Node: "credentials"},
	} {
		if got := r.report(res, nil, "uid-1").reason; got != reasonInProgress {
			t.Errorf("%s/%s: reason = %q, want %q", res.Outcome, res.Node, got, reasonInProgress)
		}
	}
}
