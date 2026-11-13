// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Engine drives kro's runtime for a single instance: it resolves, routes,
// applies and observes each node, then aggregates instance status.
type Engine struct{}

// New returns a stateless Engine.
func New() *Engine { return &Engine{} }

// Outcome says why a reconcile pass ended (ADR 0013).
type Outcome string

const (
	// OutcomeComplete: every non-ignored node was processed and is ready.
	OutcomeComplete Outcome = "Complete"
	// OutcomeNotReady: every node was processed, at least one is not ready yet.
	OutcomeNotReady Outcome = "NotReady"
	// OutcomePending: a node's dependency or data is not available yet.
	OutcomePending Outcome = "Pending"
	// OutcomeExternalNotFound: an external ref's object does not exist yet.
	OutcomeExternalNotFound Outcome = "ExternalNotFound"
	// OutcomeFailed: the pass ended on an error that is not data pending.
	OutcomeFailed Outcome = "Failed"
)

// Stage is the step of a node at which a pass ended on an error (#49).
type Stage string

const (
	// StageEvaluate: includeWhen or a template expression failed to evaluate.
	StageEvaluate Stage = "Evaluate"
	// StageRead: reading an external ref's object(s) failed.
	StageRead Stage = "Read"
	// StageApply: applying a desired object to its target failed.
	StageApply Stage = "Apply"
	// StageRoute: no applier or reader is configured for the node's target.
	StageRoute Stage = "Route"
)

// Result summarizes one reconcile pass.
type Result struct {
	// Outcome is why the pass ended; set on every return, including error returns.
	Outcome Outcome
	// Node is the node that ended the pass; for NotReady, the first node that was
	// not ready. Empty for Complete.
	Node string
	// Reason is kro's (or the API server's) message for Node. Empty for Complete.
	Reason string
	// Dependency is, for Pending, the not-ready dependency Node waits for. Empty
	// otherwise, and when the pass pends on data rather than on readiness.
	Dependency string
	// ReadyReason is the reason code of the Ready condition of the not-ready
	// object: Dependency's when set, else Node's (NotReady). Empty when that object
	// has no such condition or its reason is not a plain code (see notReadyCondition).
	ReadyReason string
	// ReadyMessage is the message of that same Ready condition, free text the
	// child's controller wrote. It goes to the instance status, never to logs.
	ReadyMessage string
	// Stage is the step at which the pass ended on an error. Empty otherwise.
	Stage Stage
	// Nodes is the number of nodes in the graph, less those found ignored this
	// pass; ReadyNodes is how many of them were processed and are ready. A pass
	// that ends early leaves the rest uncounted, so ReadyNodes < Nodes.
	Nodes, ReadyNodes int
	// Ready is true when every included node passed CheckReadiness.
	Ready bool
	// Requeue is true when a node is not yet ready or a dependency is pending.
	Requeue bool
	// Complete is true when the loop processed all non-ignored nodes without
	// early-returning on a pending dependency — safe to prune. It stays false
	// on the GetDesired pending-dependency early return, because a pending pass
	// applies only a prefix of the desired set and pruning then would delete
	// still-desired children that were simply not re-applied this pass.
	Complete bool
}

// Reconcile drives the runtime node-by-node in topological order. Each node is
// routed to a Target by the build-time routing map (keyed by resource id): a
// template node has its desired object(s) applied via the matching Applier, while
// an external node (externalRef) is read-only — its object(s) are fetched through
// the matching Reader and never applied, labeled, owned, or recorded. Either way
// the observed state feeds the runtime so later nodes' CEL resolves. It never
// rolls back: partial application is reported via Result and a requeue.
func (e *Engine) Reconcile(ctx context.Context, rt *runtime.Runtime, appliers map[Target]Applier, readers map[Target]Reader, routing map[string]Target) (Result, error) {
	nodes := rt.Nodes()
	res := Result{Ready: true, Nodes: len(nodes)}
	l := log.FromContext(ctx)
	// The Ready reason and message of every node found not ready so far this
	// pass, so a later node pending on one of them can name it and its reason.
	notReady := map[string]string{}
	notReadyMsg := map[string]string{}

	for _, node := range nodes {
		ignored, err := node.IsIgnored()
		if err != nil {
			err = res.failed(node.Spec.Meta.ID, StageEvaluate, fmt.Errorf("node %s: includeWhen: %w", node.Spec.Meta.ID, err))
			res.waitingOn(node.Spec.Meta.Dependencies, notReady, err)
			res.ReadyMessage = notReadyMsg[res.Dependency]

			return res, err
		}
		if ignored {
			res.Nodes--
			l.V(nodeLogLevel).Info("node skipped", "node", node.Spec.Meta.ID, "reason", "includeWhen false")

			continue
		}

		desired, err := node.GetDesired()
		if err != nil {
			// kro distinguishes a pending dependency (CEL couldn't resolve because
			// a referenced status/field isn't observed yet, wrapped with the
			// exported runtime.ErrDataPending sentinel) from a genuine expression
			// bug (type error, bad overload, division by zero). Only the former is
			// normal convergence flow; the latter must surface as a hard error so a
			// broken blueprint doesn't hot-loop every requeue with nothing reported.
			if errors.Is(err, runtime.ErrDataPending) {
				// A dependency is not yet observed/ready → don't apply partial;
				// converge on a later requeue. Complete stays false so the caller
				// does not prune the still-desired children this pass skipped.
				res.Ready = false
				res.Requeue = true
				res.ended(OutcomePending, node.Spec.Meta.ID, err)
				res.waitingOn(node.Spec.Meta.Dependencies, notReady, err)
				res.ReadyMessage = notReadyMsg[res.Dependency]

				//nolint:nilerr // unresolved dependency is normal convergence: signal NotReady+Requeue, not a hard error.
				return res, nil
			}

			// Genuine CEL/type error: return it so the failure is surfaced (logged,
			// counted) instead of silently requeueing forever. A hard error returns
			// before res.Complete is set — intentional; nothing should prune.
			return res, res.failed(node.Spec.Meta.ID, StageEvaluate, fmt.Errorf("node %s: %w", node.Spec.Meta.ID, err))
		}

		// Every object of a node shares the node's routing target.
		target := TargetForNode(node.Spec.Meta.ID, routing)

		//nolint:exhaustive // only external node types branch here; all other types fall through to the apply path below.
		switch node.Spec.Meta.Type {
		case graph.NodeTypeExternal:
			// An external ref is read-only: fetch the referenced object through the
			// target's Reader and feed it to the runtime. It is never applied.
			reader, ok := readers[target]
			if !ok {
				return res, res.failed(node.Spec.Meta.ID, StageRoute, fmt.Errorf("node %s: no reader configured for target %q", node.Spec.Meta.ID, target))
			}
			if len(desired) == 0 { // nothing to read (empty desired) — skip
				continue
			}
			d := desired[0]
			obj, err := reader.Get(ctx, d.GroupVersionKind(), d.GetNamespace(), d.GetName())
			if apierrors.IsNotFound(err) {
				// The external object doesn't exist yet: wait for it. Return BEFORE
				// res.Complete is set so prune stays disabled while the dependency
				// is pending — the same convergence rule as ErrDataPending above.
				res.Ready, res.Requeue = false, true
				res.ended(OutcomeExternalNotFound, node.Spec.Meta.ID, err)

				return res, nil
			}
			if err != nil {
				return res, res.failed(node.Spec.Meta.ID, StageRead, fmt.Errorf("node %s: external get: %w", node.Spec.Meta.ID, err))
			}
			observed := []*unstructured.Unstructured{obj}
			node.SetObserved(observed)
			readyErr := node.CheckReadiness()
			rr := res.notReady(notReady, notReadyMsg, node.Spec.Meta.ID, readyErr, observed)
			logNode(l, "node read", node.Spec.Meta.ID, target, d.GroupVersionKind(), observed, false, readyErr, rr)

			continue
		case graph.NodeTypeExternalCollection:
			// An external collection is read-only: list objects matching the
			// resolved selector through the target's Reader and feed them in.
			reader, ok := readers[target]
			if !ok {
				return res, res.failed(node.Spec.Meta.ID, StageRoute, fmt.Errorf("node %s: no reader configured for target %q", node.Spec.Meta.ID, target))
			}
			if len(desired) == 0 { // nothing to read (empty desired) — skip
				continue
			}
			d := desired[0]
			selector, err := externalCollectionSelector(d)
			if err != nil {
				return res, res.failed(node.Spec.Meta.ID, StageEvaluate, fmt.Errorf("node %s: %w", node.Spec.Meta.ID, err))
			}
			// Namespace comes from the resolved template; a namespaced kind with an
			// empty namespace lists across all namespaces, cluster-scoped is always "".
			ns := d.GetNamespace()
			if !node.Spec.Meta.Namespaced {
				ns = ""
			}
			items, err := reader.List(ctx, d.GroupVersionKind(), ns, selector)
			if err != nil {
				return res, res.failed(node.Spec.Meta.ID, StageRead, fmt.Errorf("node %s: external list: %w", node.Spec.Meta.ID, err))
			}
			node.SetObserved(items)
			readyErr := node.CheckReadiness()
			rr := res.notReady(notReady, notReadyMsg, node.Spec.Meta.ID, readyErr, items)
			logNode(l, "node read", node.Spec.Meta.ID, target, d.GroupVersionKind(), items, true, readyErr, rr)

			continue
		}

		// Default (Resource/Collection/Instance): apply every desired object of the
		// node to its routing target and read the result back into the runtime.
		applier, ok := appliers[target]
		if !ok {
			return res, res.failed(node.Spec.Meta.ID, StageRoute, fmt.Errorf("node %s: no applier configured for target %q", node.Spec.Meta.ID, target))
		}
		observed := make([]*unstructured.Unstructured, 0, len(desired))
		for _, obj := range desired {
			obs, err := applier.Apply(ctx, obj)
			if err != nil {
				return res, res.failed(node.Spec.Meta.ID, StageApply, fmt.Errorf("node %s: apply to %s: %w", node.Spec.Meta.ID, target, err))
			}
			observed = append(observed, obs)
		}
		collection := node.Spec.Meta.Type == graph.NodeTypeCollection
		if collection {
			alignCollectionIdentities(desired, observed)
		}
		node.SetObserved(observed)

		// A not-ready child is not an error: it exists but isn't ready yet.
		readyErr := node.CheckReadiness()
		rr := res.notReady(notReady, notReadyMsg, node.Spec.Meta.ID, readyErr, observed)
		logNode(l, "node applied", node.Spec.Meta.ID, target, node.Spec.Template.GroupVersionKind(), observed, collection, readyErr, rr)
	}

	// Every non-ignored node was resolved and applied without a pending-dependency
	// early return: the applied set is the complete desired set, so prune is safe.
	res.Complete = true
	if res.Ready {
		res.Outcome = OutcomeComplete
	} else {
		res.Outcome = OutcomeNotReady
	}

	return res, nil
}

// notReady records a node whose readiness check failed: the pass is not ready
// and requeues, the first such node is kept as the NotReady node/reason, and the
// node's Ready reason goes into seen for nodes that depend on it. It returns that
// Ready reason ("" when the node is ready).
func (r *Result) notReady(seen, seenMsg map[string]string, nodeID string, readyErr error, observed []*unstructured.Unstructured) string {
	if readyErr == nil {
		r.ReadyNodes++

		return ""
	}
	rr, msg := notReadyCondition(observed)
	seen[nodeID], seenMsg[nodeID] = rr, msg
	r.Ready, r.Requeue = false, true
	if r.Node == "" {
		r.Node, r.Reason, r.ReadyReason, r.ReadyMessage = nodeID, readyErr.Error(), rr, msg
	}

	return rr
}

// ended records why the pass stopped at nodeID, replacing any not-ready node
// recorded earlier in the pass.
func (r *Result) ended(outcome Outcome, nodeID string, err error) {
	r.Outcome, r.Node, r.Reason = outcome, nodeID, err.Error()
	r.Dependency, r.ReadyReason, r.ReadyMessage = "", "", ""
}

// waitingOn records which not-ready dependency a data-pending err waits for, and
// that dependency's Ready reason. kro names the dependency in its message
// ("dependent node %q not ready", "includeWhen dependency %q not ready"; the
// format is pinned by TestReconcile_Pending_DependencyMatchesReason). Only a
// dependency named there and found not ready in this pass is recorded, so the
// field never contradicts the reason: nothing is recorded when err is not data
// pending, or when it waits on a field rather than on a dependency's readiness.
func (r *Result) waitingOn(deps []string, notReady map[string]string, err error) {
	if !errors.Is(err, runtime.ErrDataPending) {
		return
	}
	msg := err.Error()
	for _, dep := range deps {
		if _, ok := notReady[dep]; !ok {
			continue
		}
		if strings.Contains(msg, fmt.Sprintf("dependent node %q not ready", dep)) ||
			strings.Contains(msg, fmt.Sprintf("includeWhen dependency %q not ready", dep)) {
			r.Dependency, r.ReadyReason = dep, notReady[dep]

			return
		}
	}
}

// failed records an error return at nodeID in stage and returns err unchanged. An error
// that wraps kro's ErrDataPending (e.g. includeWhen on a dependency that isn't
// ready) is still returned, but its outcome is Pending, not Failed. The pass is
// not ready either way, so its line does not claim ready=true.
func (r *Result) failed(nodeID string, stage Stage, err error) error {
	outcome := OutcomeFailed
	if errors.Is(err, runtime.ErrDataPending) {
		outcome = OutcomePending
	}
	r.ended(outcome, nodeID, err)
	r.Stage = stage
	r.Ready = false

	return err
}

// alignCollectionIdentities rewrites each desired collection item's name and
// namespace to those of the object it was applied as (observed[i] is the apply
// result of desired[i]).
//
// kro's SetObserved for a collection keeps only the observed items whose
// identity (GVK/namespace/name) matches a desired item, in desired order.
// Children renamed on their way to the target (the qualified provider/host
// names, see ProviderChildName) break that match: every item would be dropped,
// so the collection never becomes ready and everything reading it pends forever.
// Aligning the desired identities lets kro's intersection match while observed
// keeps the real, qualified names that dependents must reference.
//
// desired is kro's cached slice (GetDesired returns it without copying), so this
// updates the runtime's own view. kro reads a collection's cached desired only
// for that intersection and for the desired-vs-observed count in readiness, so
// nothing else depends on the template names. A no-op for unrenamed children.
func alignCollectionIdentities(desired, observed []*unstructured.Unstructured) {
	for i, obs := range observed {
		desired[i].SetName(obs.GetName())
		desired[i].SetNamespace(obs.GetNamespace())
	}
}

// externalCollectionSelector extracts the label selector from a resolved external
// collection's synthetic template (metadata.selector). A missing selector means
// "select everything". Mirrors kro's resolveExternalCollectionSelector.
func externalCollectionSelector(desired *unstructured.Unstructured) (labels.Selector, error) {
	raw, found, err := unstructured.NestedMap(desired.Object, "metadata", "selector")
	if err != nil || !found {
		//nolint:nilerr // mirror kro: a missing or non-map selector means "select everything".
		return labels.Everything(), nil
	}
	ls := &metav1.LabelSelector{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(raw, ls); err != nil {
		return nil, fmt.Errorf("converting external collection selector: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return nil, fmt.Errorf("invalid external collection selector: %w", err)
	}

	return selector, nil
}
