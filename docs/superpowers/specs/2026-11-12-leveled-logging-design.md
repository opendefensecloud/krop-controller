# Leveled, structured logging — design

Issue: [#72](https://github.com/opendefensecloud/krop-controller/issues/72) (dependent
children sometimes not created for 10+ minutes). Related: [#49](https://github.com/opendefensecloud/krop-controller/issues/49)
(reconcile errors never reach instance status) — not in scope here, but it reuses the
engine change below.

## Why

krop serves many blueprints (bucket, vpc, subnet, dnszone, …) from one process, and a
provider reading its logs today cannot answer the questions that matter:

- **Which instance is this about?** The only reconcile line (`reconciled; requeueing`,
  `cmd/controller/main.go`) carries `cluster` and `name` but no namespace. multicluster-
  runtime adds `controller`, `controllerGroup` and `controllerKind`, but its default log
  constructor does not add namespace/name for `mcreconcile.Request`.
- **Why did a pass stop?** `engine.Reconcile` returns `Ready=false, Requeue=true, err=nil`
  on the first pending node and drops kro's error, which already names the cause (e.g.
  `node "objectAdmin": dependent node "bucket" not ready: readyWhen … false`). In #72 that
  was the whole diagnosis, and it never reached the log.
- **What did a pass do?** Nothing records which nodes were skipped, applied, read, or
  not ready.
- **The default level is noisy.** One Info line every 30 s per instance, mostly saying
  "nothing happened".

Logs are read with `kubectl logs` on the pod and in Loki/Grafana, so lines must be both
readable and filterable by stable JSON fields.

## Goals

1. Every log line about an instance identifies it: blueprint, kind, consumer cluster,
   namespace, name.
2. Three verbosity tiers with a clear purpose each (below).
3. The default level is quiet: errors, rare lifecycle events, and deletions krop makes on
   its own.
4. Same treatment for all components: instance reconcile/engine, registrar, supervisor,
   orphan sweeper.
5. Field names and levels are documented in `docs/operations.md` so providers can build
   Loki queries against them.

## Non-goals

- Surfacing errors or pending reasons in instance **status** (#49). The engine change here
  is the foundation for it, but status/conditions/events are not touched.
- Changing engine behaviour (e.g. continuing past a pending node instead of ending the
  pass). Logging only.
- Metrics, tracing, Events.
- A new logging library or flags. Keep controller-runtime's zap and `--zap-log-level`.

## Anti-goals

- Logging values from child objects or status. The Bucket blueprint's `hmacKey` carries a
  plaintext secret in `status.secret`; no log line may include observed or desired object
  content. kro error messages are logged as-is (they name keys and expressions, not
  values) — see "Left to the builder" for the one check required. This covers what krop
  adds to logs; API-server error text that is already returned and logged today (e.g.
  SSA validation messages echoing values) is not scrubbed here. #49 must address it
  before such text reaches instance status (builder's call on scope).
- A default level that still emits a line per instance per pass.
- Field names that differ between components for the same thing.

## Verbosity tiers (decided)

zap mapping: `--zap-log-level=info` (default) shows Info; `debug` shows `V(1)`; an integer
level `2` shows `V(2)`. Document this mapping, since `V(2)` has no named zap level.

| Tier | Purpose | Instance reconcile / engine | Registrar | Supervisor | Sweeper |
|---|---|---|---|---|---|
| **default (Info + Error)** | errors, rare lifecycle, deletions krop decides on | reconcile errors (with instance fields); **prune deletes**, one line per pass and target listing the deleted children | publish failure (the reason it already writes to status + error); APIExport published/republished when the spec hash changed; APIExport deleted with its blueprint | manager started / stopped per export; manager crashed (exists) | **orphan children deleted**, one line per target; sweep pass failed (exists); orphaned instance detected (exists) |
| **V(1)** | why each pass ended | one line per pass: outcome, ending node, kro's reason | unchanged resync ("already published") | — | starting sweeper; startup-grace skip (exists) |
| **V(2)** | what each pass did | one line per node: skipped (includeWhen) / applied / read / not ready, target, final child name | — | `Ensure` no-op (already running) | per-record "still fresh" decisions |

The `reconciled; requeueing` / `reconciled` lines leave the default level; the V(1)
pass-end line replaces them.

Instance deletion (finalizer path) child deletes are deletions krop makes on its own and
are logged at default, same as prune.

Deletions are logged as one line per pass and target, not per child: below V(2),
controller-runtime's production logger samples each message (100 per second, then 1 in
100), so per-child lines would be dropped when many instances are deleted at once. The
binary defaults `--zap-stacktrace-level` to `panic`: controller-runtime's production
default puts a Go stack trace on every error line, and krop's errors are reconcile
errors whose message already says what failed.

## Fields

Instance-scoped lines carry, from a logger built once per request and passed via context:

| Key | Value |
|---|---|
| `blueprint` | the blueprint (RGD) object name, e.g. `bucket` — what providers call it |
| `export` | the APIExport name (`<plural>.<group>`, today's `Reconciler.BlueprintName`) |
| `kind` | instance kind (e.g. `Bucket`) |
| `cluster` | consumer logical cluster |
| `namespace` | instance namespace (empty for cluster-scoped) |
| `name` | instance name |
| `instanceUID` | instance UID, added once the instance is read (after the `Get`), so instance lines join with sweeper lines |

`blueprint` always means the RGD name, in every component (builder's call). The instance
path does not know it today: thread it from the registrar through `OnPublished` and the
served-blueprint registry into the `Reconciler`. Do **not** change the `blueprint` GC
label value stamped on children (persisted, used by GC); only the log field changes.
The registrar logs `blueprint` always and `export` once it is known (not on early
failures such as `HashFailed` / `BuildFailed`).

Install the instance logger with the controller builder's `WithLogConstructor`, not
inside the reconcile func: controller-runtime builds the logger for its "Reconciler
error" line from `LogConstructor` before calling the reconciler, so only that placement
puts the instance fields on error lines.

Per-pass and per-node lines add:

| Key | Value |
|---|---|
| `outcome` | `Complete` (finished, all ready), `NotReady` (finished, ≥1 node not ready — the engine never ends a pass on readiness), `Pending`, `ExternalNotFound`, `Failed` (pass-end line) |
| `node` | kro resource id (e.g. `objectAdmin`) |
| `target` | `consumer` / `provider` / `host` |
| `gvk` | child GVK, `group/version/Kind` |
| `child` | final child name after qualification (and `childNamespace` if namespaced) |
| `reason` | kro's error/readiness message, as a string |

Components that are not instance-scoped use `export` (supervisor), `blueprint`
(registrar), and `record` / `instanceUID` / `cluster` (sweeper). The liveness record
holds no kind, namespace or name, and this work does not extend it (persisted data);
sweeper lines join to instance lines via `instanceUID`. Keys are lowerCamelCase,
consistent with the existing `apiExport`, `instanceUID`, `staleAfter`. Where an existing
key for the same thing differs (e.g. supervisor's `export` vs startup's `apiExport`),
pick one and use it everywhere.

## Engine change (decided in outline)

`engine.Result` gains the information the pass-end line needs and #49 will later reuse:
the outcome, the node that ended the pass (or the first not-ready node when the pass
completed), and the underlying error/reason. The engine itself logs V(2) per-node lines
using the logger from `ctx`. The reconciler/entrypoint logs the V(1) pass-end line from
`Result`. Error paths keep returning errors as today (controller-runtime's "Reconciler
error" line then carries the instance fields via the context logger).

**includeWhen pending (decided).** kro's `IsIgnored` wraps `ErrDataPending` when an
`includeWhen` reads a dependency that isn't ready, but `engine.Reconcile` returns every
`IsIgnored` error as a hard error (exponential backoff, a default-level "Reconciler
error" on every retry). This work does **not** change that behaviour: it records the
outcome as `Pending` with the reason (`errors.Is(err, runtime.ErrDataPending)`) while
still returning the error. A separate bugfix PR changes the engine to requeue it like the
`GetDesired` pending path.

## Constraints

- Keep `go test`/Ginkgo + envtest conventions; tests assert on the new `Result` fields
  rather than on log output where possible.
- Minimal diff outside logging: no unrelated refactors.
- Logger plumbing via `logr` from context (`log.FromContext` / `log.IntoContext`), no
  globals beyond what exists.

## Left to the builder

- Exact message strings, and whether the instance logger is installed via the controller
  builder's `WithLogConstructor` or built inside the reconcile func.
- Exact `Result` field names/types and whether a not-ready node in a completed pass is
  recorded (one is enough for the V(1) line).
- Whether V(2) per-node logging lives in the engine or is driven from the reconciler.
- Verify kro error strings carry no object values in the paths logged (pending, not ready,
  external not found); if any does, log a trimmed form.
- Updating `docs/operations.md` Observability with the tier table, field list, zap level
  mapping, and two example Loki queries (one instance's history; all pending passes for a
  blueprint).
