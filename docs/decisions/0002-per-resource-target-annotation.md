# 2. Route children by a per-resource annotation on a kro-identical RGD

## Status

**Superseded by [0011](0011-external-refs-and-host-target.md).** The
`krop.opendefense.cloud/target` annotation described here has been replaced by a
per-resource `target` **field** on a thin krop wrapper spec (and the target set
grew from `consumer|provider` to `consumer|provider|host`). The historical body
below is kept for context.

## Context

A blueprint must express, per child resource, whether it lands in the
**consumer** workspace or the **provider** workspace. kro's RGD has no such field.
Two shapes were considered:

1. A superset CRD (e.g. `CompositionBlueprint`) that embeds a kro RGD and adds a
   structured `target` field (and an explicit `consumerClaims` list) per resource.
2. A per-resource **annotation** on the kro resource template, stripped before
   apply, keeping the authored artifact a raw kro RGD.

The original design leaned toward the superset CRD for legibility but left this an
explicit implementer decision.

## Decision

Serve krop's own cluster-scoped CRD `krop.opendefense.cloud/v1alpha1`
`ResourceGraphDefinition` (shortName `rgd`) whose **spec is kro's
`ResourceGraphDefinitionSpec` verbatim** (`api/v1alpha1/resourcegraphdefinition_types.go`).
Routing is carried by the annotation `krop.opendefense.cloud/target: consumer|provider`
on each resource template. `consumer` is the default when the annotation is absent.

The engine reads the target off each desired object (`TargetOf`) and **strips the
annotation** (`StripRouting`) before apply so it never reaches kro or the
materialized object (`internal/engine/route.go`).

## Consequences

- The blueprint parses into kro's type **unchanged** — no translation layer, and
  kro's builder/runtime see a pristine RGD.
- Authors write ordinary kro RGDs plus one annotation; existing kro tooling and
  docs apply directly.
- The routing annotation is engine-only: an unknown value is a hard error, and the
  annotation is removed before SSA so it cannot leak onto children.
- The blueprint is served under krop's *own* group/kind (not kro's), so it can add
  a krop-owned status and be reconciled by krop's Registrar without colliding with
  a kro installation.
- Consumer claims are **not** hand-listed in the blueprint (they are derived — see
  ADR 0004), so the superset CRD's `consumerClaims` field is unnecessary.
</content>
