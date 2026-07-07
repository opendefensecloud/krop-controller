# krop-controller docs

- [permissions.md](permissions.md) — authorization & least-privilege permission
  model: the three-tier posture (hosting cluster ServiceAccount only, kcp root
  workspace, kcp provider workspace, consumer workspaces), how the controller's
  kcp identity is minted, how to apply the RBAC fixtures in `config/kcp/rbac/`,
  and the permissionClaims spine for cross-workspace writes.
- [superpowers/specs/](superpowers/specs/) — design specs (see the M6 design doc
  §9 for provider vs consumer ownership domains).
- [superpowers/plans/](superpowers/plans/) — per-milestone implementation plans.
