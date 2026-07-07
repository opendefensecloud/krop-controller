# 3. Publish the generated instance CRD as an APIResourceSchema via the kcp SDK

## Status

Accepted.

## Context

kro's graph builder synthesizes a full CRD for the instance kind from the
blueprint's SimpleSchema (`Graph.CRD`). In kcp you do not serve a CRD to make a
type bindable — you publish an `APIResourceSchema` referenced by an `APIExport`,
and consumers bind it. krop needs to convert kro's generated CRD into that kcp
shape, stably and idempotently.

## Decision

Use the kcp SDK's `apisv1alpha1.CRDToAPIResourceSchema(g.CRD, "v"+specHash)` to
convert the generated CRD into an `APIResourceSchema`
(`internal/registrar/publish.go`, `BuildARS`). The helper names the object
`<prefix>.<crd.Name>`, so the `"v"+specHash` prefix yields the
`v<specHash>.<plural>.<group>` naming convention that matches kcp's own scheme.

An ARS is **immutable once served**, and a new spec mints a new specHash → a new
ARS name, so the Registrar **creates-if-absent** and never patches an existing ARS
(`applyARS`). The APIExport is server-side-applied to reference the current ARS.

## Consequences

- The published type is derived deterministically from the blueprint, and its ARS
  name encodes the exact spec it reflects.
- No custom CRD→ARS conversion code to maintain; krop tracks the kcp SDK's
  conversion semantics.
- Because the ARS is immutable, a spec change produces a *new* ARS (see ADR 0009
  for how instances are re-served); old ARS objects are cascade-deleted only on
  blueprint withdrawal.
- On blueprint delete, the Registrar cascade-unpublishes: it deletes the
  referenced ARS(es) *before* the APIExport so a mid-teardown resync never observes
  an APIExport pointing at a missing schema (`DeletePublishedAPI`).
</content>
