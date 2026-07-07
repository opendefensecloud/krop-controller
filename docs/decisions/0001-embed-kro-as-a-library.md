# 1. Embed kro as a library, not as a controller

## Status

Accepted.

## Context

kro (`github.com/kubernetes-sigs/kro`, v0.9.2) already solves the hard parts of
declarative composition: a SimpleSchema shorthand that generates an instance CRD,
a CEL expression language with build-time type-checking against child schemas, a
DAG builder with cycle detection, and a **client-free runtime** (`FromGraph` →
per-node `GetDesired`/`SetObserved`/`CheckReadiness`/`IsIgnored`) that resolves
desired objects and checks readiness without ever touching a cluster.

But kro's *runtime wiring* is fundamentally single-cluster: it creates a local CRD
plus a per-RGD micro-controller to serve the generated API, applies every child
into the one cluster it runs against via an ApplySet + dynamic client, and watches
via per-GVR dynamic informers. None of that fits kcp, where the API is published
as an APIExport (not a CRD), consumers span many logical clusters behind a virtual
workspace, and children must be split across two workspaces.

A prior spike confirmed vanilla kro *can* reconcile against a single kcp workspace
endpoint unmodified, but that gives one deployment per tenant workspace and no
dual-target placement.

## Decision

Import kro's pure/engine layers as a library and drop its controller layers.

- **Use:** `api/v1alpha1` (RGD types), `pkg/simpleschema`, `pkg/graph` +
  `pkg/graph/crd` (build the graph and the generated CRD), `pkg/cel`, and
  `pkg/runtime` (the client-free reconcile-one-instance core).
- **Drop / re-implement:** `pkg/controller/instance` (single-cluster apply/prune/
  GC), `pkg/controller/resourcegraphdefinition` (local CRD + micro-controller),
  and `pkg/dynamiccontroller` (single-cluster watch coordinator).

krop owns all I/O: it feeds observed state into the runtime via `SetObserved` and
takes bare `unstructured` objects out of `GetDesired`, choosing the target
endpoint per object itself.

## Consequences

- Dual-target apply needs **no kro fork**: `pkg/runtime` imports no client-go /
  controller-runtime / dynamic client, so krop can route each desired object to a
  different workspace with an unmodified engine.
- krop reuses kro's CEL/DAG/schema semantics exactly, so blueprint authoring
  matches kro's documented surface.
- krop must re-implement apply, prune, finalizer GC, and status projection (the
  dropped `pkg/controller/instance` layer) as its own dual-target reconcile loop.
- krop is pinned to kro v0.9.2's package layout (no `internal/` packages block the
  imports); a kro upgrade must be validated against these entry points.
</content>
