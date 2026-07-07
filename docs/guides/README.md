# Guides

Task-oriented how-tos for krop-controller. Each guide is a focused, standalone
recipe; for the "why" and the full reference, they link out to
[architecture.md](../architecture.md), [blueprints.md](../blueprints.md),
[permissions.md](../permissions.md), and [operations.md](../operations.md) rather
than repeating them.

New to krop? Do the end-to-end [getting-started walkthrough](../getting-started.md)
first — it installs the controller and runs the full provider/consumer flow.

| Guide | When to use it |
| --- | --- |
| [Writing your first blueprint](writing-your-first-blueprint.md) | You want the gentlest on-ramp: build, publish, bind, and consume a **minimal single-child** blueprint from scratch, with each piece explained. |
| [Cross-target dependencies](cross-target-dependencies.md) | You need a consumer child to consume a **provider child's status** via CEL (the pend-until-ready pattern). |
| [Deploying in production](deploying-in-production.md) | You are deploying krop for real: mint the identity, apply least-privilege RBAC, `helm install` with HA + metrics. |
| [Troubleshooting](troubleshooting.md) | Something isn't materializing and you need a problem → cause → fix checklist. |

For the complete authoring surface (SimpleSchema, `readyWhen`/`includeWhen`/
`forEach`, pruning, schema evolution) see the
[blueprint authoring reference](../blueprints.md).
