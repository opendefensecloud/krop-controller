# krop-controller docs

krop-controller is a kcp-native composition controller: a provider authors one
declarative `ResourceGraphDefinition` blueprint, krop publishes it as a bindable
`APIExport`, and consumers bind + create instances whose child resources are
materialized across the consumer and provider workspaces — no Go controller per
offering.

For the project landing page (quickstart, feature overview, install), see the
[repository README](../README.md).

## Start here

- [getting-started.md](getting-started.md) — install the controller, mint its kcp
  identity, publish your first blueprint, bind a consumer, and create an instance.
- [blueprints.md](blueprints.md) — authoring `ResourceGraphDefinition` blueprints:
  schema, resources, CEL, and consumer/provider target routing.
- [operations.md](operations.md) — running krop in production: deployment,
  upgrades, observability, and troubleshooting.

## Guides

Task-oriented how-tos (recipes and tutorials) — see [guides/](guides/) for the
index:

- [guides/writing-your-first-blueprint.md](guides/writing-your-first-blueprint.md)
  — a from-scratch tutorial building a minimal single-child blueprint.
- [guides/cross-target-dependencies.md](guides/cross-target-dependencies.md) —
  the provider-status → consumer-child CEL recipe (pend-until-ready).
- [guides/deploying-in-production.md](guides/deploying-in-production.md) — a
  concise deploy recipe: identity, RBAC, `helm install` with HA + metrics.
- [guides/troubleshooting.md](guides/troubleshooting.md) — problem → cause → fix
  with concrete `kubectl` checks.

## Reference

- [architecture.md](architecture.md) — how krop is built and why: overview &
  motivation, workspace topology, components (Registrar, Supervisor, Engine +
  applier chain, Reconciler, Sweeper, kcp helpers, blueprint CRD), the key flows
  (publish, dual-target instance reconcile, garbage collection), the permission
  model, testing tiers, and known limitations. Includes Mermaid diagrams.
- [decisions/](decisions/) — architecture decision records: the significant design
  choices (kro-as-library, target-annotation routing, APIExport publication,
  auto-derived claims, the Supervisor, the graph cache, cross-workspace GC,
  least-privilege, change-detected restart, and the envtest tier).
- [permissions.md](permissions.md) — authorization & least-privilege model: the
  three-tier posture (host cluster ServiceAccount only, kcp root workspace, kcp
  provider workspace, consumer workspaces), how the controller's kcp identity is
  minted, how to apply the RBAC fixtures in `config/kcp/rbac/`, and the
  permissionClaims spine for cross-workspace writes.
</content>
