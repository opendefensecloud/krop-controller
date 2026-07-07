# krop-controller docs

krop-controller is a kcp-native composition controller: a provider authors one
declarative `ResourceGraphDefinition` blueprint, krop publishes it as a bindable
`APIExport`, and consumers bind + create instances whose child resources are
materialized across the consumer and provider workspaces — no Go controller per
offering.

## Start here

- [getting-started.md](getting-started.md) — install the controller, mint its kcp
  identity, publish your first blueprint, bind a consumer, and create an instance.
- [blueprints.md](blueprints.md) — authoring `ResourceGraphDefinition` blueprints:
  schema, resources, CEL, and consumer/provider target routing.
- [operations.md](operations.md) — running krop in production: deployment,
  upgrades, observability, and troubleshooting.

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
