# Leveled, Structured Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give providers quiet default logs (errors, lifecycle, deletions), a V(1) line per instance pass saying why it ended, and V(2) per-node detail, all with one field contract.

**Architecture:** The engine records the pass outcome in `engine.Result` and emits V(2) per-node lines through the context logger. The instance reconciler emits the V(1) pass-end line and default-level deletion lines. The entrypoint installs per-request loggers (instance and registrar controllers) via `WithLogConstructor` so controller-runtime's own error lines carry the fields. Registrar, supervisor and sweeper move existing lines to the agreed tiers.

**Tech Stack:** Go, controller-runtime / multicluster-runtime logr+zap, kro v0.9.4 runtime, `github.com/go-logr/logr/funcr` for log capture in tests.

**Spec:** `docs/superpowers/specs/2026-11-12-leveled-logging-design.md` (ADR: `docs/decisions/0013-leveled-structured-logging.md`)

## Global Constraints

- Branch `feature/72/leveled-logging`. Never commit directly: each task ends by staging and proposing a `gitcs '<conventional commit>'` line for the user to run. No AI attribution anywhere.
- Tiers: default = Info/Error; `V(1)` = pass outcome; `V(2)` = per-node / per-record detail.
- Field keys (lowerCamelCase, identical everywhere): `blueprint` (RGD name), `export` (APIExport name), `kind`, `cluster`, `namespace`, `name`, `instanceUID`, `outcome`, `node`, `target`, `gvk` (`group/version/Kind`, core group as `/v1/Kind`), `child`, `childNamespace`, `children`, `reason`, `ready`, `record`.
- Outcome values exactly: `Complete`, `NotReady`, `Pending`, `ExternalNotFound`, `Failed`.
- Never log observed or desired object content. Only names, GVKs, targets, and kro/apiserver error strings.
- No reconcile behaviour change. includeWhen data-pending keeps returning an error; only its recorded outcome is `Pending`.
- The `blueprint` GC label value on children (export name) is unchanged.
- Unit tests follow the package's existing style (plain `testing` + `t.Fatalf` in unit files, Ginkgo/Gomega in envtest suites). Minimal diff: no unrelated refactors.

## Review Focus

1. A child carrying a secret in status (Bucket's `hmacKey.status.secret`) must never have that value in any log line, at any verbosity. Test: Task 1 Step 1 `TestReconcile_V2Lines_NeverLogObjectContent`.
2. controller-runtime calls `LogConstructor(nil)` at controller start. The instance constructor must not panic and must still carry `blueprint`/`export`/`kind`. Test: Task 2 Step 1 `TestInstanceLogConstructor_NilRequest`.
3. Cluster-scoped instances have an empty namespace; lines must still carry `namespace: ""` and not drop the key. Test: Task 2 Step 1 `TestInstanceLogConstructor_FieldsFromRequest` (cluster-scoped case).
4. An empty forEach collection (zero children) must log a V(2) line without indexing `observed[0]`. Test: Task 1 Step 1 `TestReconcile_V2Line_EmptyCollection`.
5. A pass that returns an error must still produce the V(1) pass-end line (`Failed`, or `Pending` for includeWhen data pending) before the error is returned. Tests: Task 1 `TestReconcile_IncludeWhenPending_OutcomePendingStillErrors`, Task 2 `TestReconcile_PassEndLine_OnError`.

---

## File Structure

| File | Change |
|---|---|
| `internal/testlog/testlog.go` (new) | test-only funcr capture sink shared by all packages |
| `internal/engine/engine.go` | `Outcome` type, `Result.Outcome/Node/Reason`, V(2) node lines |
| `internal/engine/logging.go` (new) | `GVKString`, node-line helper (keeps engine.go focused) |
| `internal/controller/logging.go` (new) | `InstanceLogConstructor` |
| `internal/controller/reconciler.go` | `instanceUID` on ctx logger, V(1) pass-end line, deletion Info lines |
| `internal/registrar/registrar.go` | `OnPublished` gains blueprint name; published/unchanged lines; reason-wrapped errors |
| `internal/registrar/dynamic_harness_test.go` | adapt `OnPublished` call |
| `internal/supervisor/supervisor.go` | `Log` field; start/stop Info, Ensure no-op V(2) |
| `internal/controller/sweeper.go` | tier moves; field renames on child-deleted line |
| `cmd/controller/main.go` | `servedBlueprint.name`, log constructors, drop `reconciled` lines, `apiExport`→`export` |
| `docs/operations.md` | Observability: tiers, fields, zap levels, Loki examples |

---

### Task 1: Engine outcome and V(2) node lines

**Files:**
- Create: `internal/testlog/testlog.go`, `internal/engine/logging.go`, `internal/engine/logging_test.go`
- Modify: `internal/engine/engine.go`

**Interfaces:**
- Produces (testlog):
  ```go
  type Line struct { Msg string; Level int; Err string; Values map[string]any }
  type Sink struct { /* mutex-guarded []Line */ }
  func New(verbosity int) (logr.Logger, *Sink)   // funcr.NewJSON, Options{Verbosity: verbosity}
  func (s *Sink) Lines() []Line                    // copy
  func (s *Sink) Find(msg string) []Line
  func (s *Sink) Raw() string                      // all JSON lines joined, for substring checks
  ```
  `Level` is funcr's `"level"` value (V level); error lines have `Err` set and `Level` 0.
- Produces (engine):
  ```go
  type Outcome string
  const (OutcomeComplete Outcome = "Complete"; OutcomeNotReady = "NotReady"; OutcomePending = "Pending"; OutcomeExternalNotFound = "ExternalNotFound"; OutcomeFailed = "Failed")
  // Result gains:
  Outcome Outcome // why the pass ended; set on every return, including error returns
  Node    string  // node that ended the pass; for NotReady, the first not-ready node; "" for Complete
  Reason  string  // kro/apiserver error or readiness message for Node; "" for Complete
  func GVKString(gvk schema.GroupVersionKind) string // "group/version/Kind", core → "/v1/Kind"
  ```

- [ ] **Step 1: Write the failing tests** in `internal/engine/logging_test.go` (plain `testing`, reuse `newRuntime`, `crossTargetRuntime`, `externalRefRuntime`, `brokenCELRGD`, `reconcileCollection`, `fakeApplier`, `fakeReader`). Inject the logger with `logr.NewContext(context.Background(), logger)` from `testlog.New(2)`.
  - `TestReconcile_Outcome_Complete`: sample RGD → `res.Outcome == OutcomeComplete`, `res.Node == ""`, `res.Reason == ""`.
  - `TestReconcile_Outcome_PendingNamesNode`: `crossTargetRuntime`, provider without status → `OutcomePending`, `Node == "subnet"`, `Reason` contains `"vpc"` and `"not ready"`.
  - `TestReconcile_Outcome_ExternalNotFound`: `externalRefRuntime` + empty `fakeReader` → `OutcomeExternalNotFound`, `Node` = the external node id, `Reason` non-empty.
  - `TestReconcile_Outcome_NotReadyFirstNode`: a new RGD with only the cross-target `vpc` node (readyWhen `${vpc.status.vpcID != ''}`, routed to provider, no dependents), provider without status → `OutcomeNotReady`, `Node == "vpc"`, `Reason` contains `readyWhen`, `res.Complete == true`.
  - `TestReconcile_Outcome_FailedOnGenuineError`: `brokenCELRGD` → error returned, `res.Outcome == OutcomeFailed`, `res.Node` = the broken node id, `res.Reason == err.Error()`-derived (non-empty).
  - `TestReconcile_IncludeWhenPending_OutcomePendingStillErrors`: cross-target RGD variant where `subnet` has includeWhen `${vpc.status.vpcID != ''}` and `vpc` has readyWhen `${vpc.status.vpcID != ''}`; provider without status → `err != nil` (behaviour unchanged), `errors.Is(err, runtime.ErrDataPending)`, `res.Outcome == OutcomePending`, `res.Node == "subnet"`.
  - `TestReconcile_V2Lines_PerNode`: sample RGD at V(2) → `sink.Find("node applied")` has 2 lines with `node` ∈ {`config`,`providerRecord`}, `target` ∈ {`consumer`,`provider`}, `gvk == "/v1/ConfigMap"`, `child == "eu-cluster-config"` for `config`, `ready == true`. At `testlog.New(1)` the same pass yields 0 `node applied` lines.
  - `TestReconcile_V2Lines_ExternalRead`: external ref found → one `node read` line with `node`, `target`, `child`.
  - `TestReconcile_V2Line_EmptyCollection`: `reconcileCollection(t)` with no names, logger at V(2) → no panic, one `node applied` line for the collection node with `children` an empty list.
  - `TestReconcile_V2Lines_NeverLogObjectContent`: sample RGD with consumer applier `mutate` setting `status.secret = "SENTINEL-SECRET-42"` and `data.region` input `"eu"`; at V(2) `sink.Raw()` does not contain `SENTINEL-SECRET-42`.
  - `TestGVKString`: `{Group:"", Version:"v1", Kind:"ConfigMap"}` → `"/v1/ConfigMap"`; `{"storage.cnrm.cloud.google.com","v1beta1","StorageBucket"}` → `"storage.cnrm.cloud.google.com/v1beta1/StorageBucket"`.

- [ ] **Step 2: Run tests to verify they fail**

  Run: `go test ./internal/engine/ -run 'TestReconcile_Outcome|TestReconcile_IncludeWhen|TestReconcile_V2|TestGVKString'`
  Expected: compile FAIL (`undefined: OutcomeComplete`, `undefined: testlog`).

- [ ] **Step 3: Implement `internal/testlog/testlog.go`** using `funcr.NewJSON(fn, funcr.Options{Verbosity: v})`; `fn` unmarshals each JSON line into a `Line` (keys `msg`, `level`, `error`; everything else into `Values`) under a mutex. Add the license header. Run `go mod tidy` (promotes `github.com/go-logr/logr` to a direct dependency).

- [ ] **Step 4: Implement engine changes**
  - `internal/engine/logging.go`: `GVKString`, and `logNode(l logr.Logger, msg, nodeID string, target Target, gvk schema.GroupVersionKind, observed []*unstructured.Unstructured, collection bool, readyErr error)`. Single → `child` (+ `childNamespace` when non-empty); collection → `children` ([]string of names, `[]string{}` when empty). Always `ready` (bool); `reason` only when `readyErr != nil`. Names only, never `.Object`.
  - `engine.go`: `l := log.FromContext(ctx)` (controller-runtime `pkg/log`). Every `return` sets `Outcome/Node/Reason`:
    - ErrDataPending from `GetDesired` → `Pending`, node, `err.Error()`.
    - External not found → `ExternalNotFound`, node, `err.Error()`.
    - Any error return → `Pending` if `errors.Is(err, runtime.ErrDataPending)`, else `Failed`. Node = current node id, Reason = the wrapped error's `Error()`. Use one small helper (`failed(res *Result, nodeID string, err error) error`) to avoid repeating this at each return.
    - Loop end → `Complete` if `res.Ready`, else `NotReady` with the first not-ready node and its `CheckReadiness` error message (record only the first). An early return later in the pass overwrites a recorded not-ready node with its own node/reason.
  - V(2) lines: `l.V(2).Info("node skipped", "node", id, "reason", "includeWhen false")` for ignored nodes. `node read` after an external Get/List is observed. `node applied` after `SetObserved` of an applied node. Each passes the `CheckReadiness` error as `readyErr`.

- [ ] **Step 5: Run the engine package**

  Run: `go test ./internal/engine/...`
  Expected: PASS, including all existing tests (behaviour unchanged).

- [ ] **Step 6: Stage and propose commit**

  ```bash
  git add internal/testlog internal/engine go.mod go.sum
  ```
  Propose: `gitcs 'feat(engine): record pass outcome and log per-node detail at V(2) (#72)'`

---

### Task 2: Reconciler pass-end line, instance logger, deletion lines

**Files:**
- Create: `internal/controller/logging.go`, `internal/controller/logging_test.go`
- Modify: `internal/controller/reconciler.go`

**Interfaces:**
- Consumes: `engine.Result{Outcome, Node, Reason}`, `engine.GVKString`, `testlog.New`.
- Produces:
  ```go
  // InstanceLogConstructor returns a multicluster-runtime LogConstructor that tags
  // every reconcile line with the blueprint/export/kind and the request's cluster,
  // namespace and name. Safe for a nil request.
  func InstanceLogConstructor(base logr.Logger, blueprint, export, kind string) func(*mcreconcile.Request) logr.Logger
  ```
  `mcreconcile` = `sigs.k8s.io/multicluster-runtime/pkg/reconcile`. Non-nil request adds `cluster` (`string(req.ClusterName)`), `namespace`, `name`.

- [ ] **Step 1: Write the failing tests**
  - `logging_test.go`:
    - `TestInstanceLogConstructor_NilRequest`: `InstanceLogConstructor(lg, "bucket", "buckets.storage.x", "Bucket")(nil).Info("x")` → line has `blueprint`, `export`, `kind`, and no `name` key; no panic.
    - `TestInstanceLogConstructor_FieldsFromRequest`: request `{ClusterName:"c1", Namespace:"ns", Name:"b1"}` → `cluster=="c1"`, `namespace=="ns"`, `name=="b1"`. Cluster-scoped case `Namespace:""` → key `namespace` present with `""`.
  - In `reconciler_conditions_test.go` style (fake clients, `buildConditionsGraph`), new tests in `logging_test.go`:
    - `TestReconcile_PassEndLine_Complete`: ctx logger `testlog.New(1)` → exactly one `pass ended` line at level 1 with `outcome=="Complete"`, `ready==true`, and `instanceUID=="uid-1"`. With `testlog.New(0)` → zero `pass ended` lines.
    - `TestReconcile_PassEndLine_OnError`: Reconciler with `HostClient: nil` and `Routing` sending `config` to `host` (missing applier → engine error) → `err != nil` and one `pass ended` line with `outcome=="Failed"`, `node=="config"`, non-empty `reason`.
  - Extend `TestPruneChildren_DeletesStaleKeepsApplied` (`reconciler_test.go`): call with a `testlog.New(0)` ctx and assert one `pruned child` line at level 0 with `target`, `gvk`, `child` = the stale child name, and no line for the kept child.
  - `TestDeleteChildren_LogsEachDelete` (in `reconciler_test.go`, fake clients like `TestPruneChildren_DeletesStaleKeepsApplied`): one labeled child present → one `deleted child` line at level 0 with `target`, `gvk`, `child`.

- [ ] **Step 2: Run to verify failure**

  Run: `go test ./internal/controller/ -run 'TestInstanceLogConstructor|TestReconcile_PassEndLine|TestPruneChildren|TestDeleteChildren'`
  Expected: compile FAIL (`undefined: InstanceLogConstructor`), then assertion failures.

- [ ] **Step 3: Implement**
  - `logging.go`: `InstanceLogConstructor` as specified.
  - `reconciler.go`:
    - After the instance `Get`: `l := log.FromContext(ctx).WithValues("instanceUID", string(inst.GetUID())); ctx = log.IntoContext(ctx, l)`.
    - Right after `kropengine.New().Reconcile(...)`, before the `err` check: `l.V(1).Info("pass ended", "outcome", res.Outcome, "node", res.Node, "reason", res.Reason, "ready", res.Ready)`.
    - `pruneChildren`: after a successful `Delete`, `Info("pruned child", "target", target, "gvk", kropengine.GVKString(gvk), "child", name, "childNamespace", ns)`. `deleteChildren`: same fields, message `deleted child`. Use `log.FromContext(ctx)`.

- [ ] **Step 4: Run the package**

  Run: `go test ./internal/controller/ -run 'Test[^C]'` (unit tests; the envtest suite `TestControllerIntegration` runs in Task 7)
  Expected: PASS.

- [ ] **Step 5: Stage and propose commit**

  ```bash
  git add internal/controller/logging.go internal/controller/logging_test.go internal/controller/reconciler.go internal/controller/reconciler_test.go
  ```
  Propose: `gitcs 'feat(controller): log pass outcome at V(1) and krop-initiated deletions (#72)'`

---

### Task 3: Registrar tiers and blueprint name in OnPublished

**Files:**
- Modify: `internal/registrar/registrar.go`, `internal/registrar/dynamic_harness_test.go`, `internal/registrar/publish_e2e_test.go`, `cmd/controller/main.go` (only the `OnPublished` closure signature, new arg unused until Task 6, so every commit builds)

**Interfaces:**
- Produces: `OnPublished func(blueprint, exportName string, instanceGVK schema.GroupVersionKind, g *graph.Graph, routing map[string]kropengine.Target, changed bool)`. `blueprint` is `bp.Name`.

- [ ] **Step 1: Write the failing test.** In `publish_e2e_test.go` (Ginkgo, reuse the existing `reg.Reconcile(ctx, reconcile.Request{…})` setup near line 136), add `It("logs publish at default level only when the spec changed")`. Drive Reconcile twice with `ctx = logr.NewContext(ctx, lg)` where `lg, sink := testlog.New(1)`. Expect exactly one `published APIExport` line at level 0 with `blueprint` = the RGD name, `export` = the export name and `specHash`. The second, unchanged pass adds one `APIExport unchanged` line at level 1 and no new level-0 line. Also update the `OnPublished` literal in `dynamic_harness_test.go` to the new signature. Assert the received `blueprint` arg equals the RGD name in whichever existing harness test inspects the callback; otherwise ignore it.

- [ ] **Step 2: Run to verify failure**

  Run: `make test` (envtest). Or, if assets are already set up: `ginkgo -focus 'logs publish' ./internal/registrar/`
  Expected: FAIL (no such log line / signature mismatch).

- [ ] **Step 3: Implement** in `registrar.go`:
  - Pass `bp.Name` as the first `OnPublished` argument.
  - After the status update: `if changed { l.Info("published APIExport", "blueprint", bp.Name, "export", exportName, "specHash", specHash) } else { l.V(1).Info("APIExport unchanged", …same keys) }`.
  - `fail`: keep writing `err.Error()` to the status message unchanged. Return `fmt.Errorf("%s: %w", reason, err)` so controller-runtime's default-level error line names the stage.

- [ ] **Step 4: Run** `go build ./... && make test` → builds; registrar suite PASS.

- [ ] **Step 5: Stage and propose commit**

  ```bash
  git add internal/registrar cmd/controller/main.go
  ```
  Propose: `gitcs 'feat(registrar): log publishes at default and resyncs at V(1) (#72)'`

---

### Task 4: Supervisor lifecycle lines

**Files:**
- Modify: `internal/supervisor/supervisor.go`, `internal/supervisor/supervisor_test.go`

**Interfaces:**
- Produces: exported field `Log logr.Logger` on `Supervisor`. `New` sets it to `logf.Log.WithName("supervisor")`. Callers and tests may override it.

- [ ] **Step 1: Write the failing test** `TestSupervisor_LogsStartStopAndNoop`: `s := New(blockingStart)`; `lg, sink := testlog.New(2)`; `s.Log = lg`. `Ensure` twice, then `Stop`. Expect one `starting instance manager` (level 0, `export=="export-a"`), one `instance manager already running` (level 2), and one `stopping instance manager` (level 0). `Stop` of an unknown export logs nothing.

- [ ] **Step 2: Run** `go test ./internal/supervisor/ -run TestSupervisor_LogsStartStopAndNoop -race` → FAIL.

- [ ] **Step 3: Implement.** Add the lines in `Ensure` and `Stop`, and switch the crash `Error` to `s.Log`. Keep the key `export`.

- [ ] **Step 4: Run** `go test ./internal/supervisor/... -race` → PASS.

- [ ] **Step 5: Stage and propose commit**

  ```bash
  git add internal/supervisor
  ```
  Propose: `gitcs 'feat(supervisor): log instance manager start and stop (#72)'`

---

### Task 5: Sweeper tiers and fields

**Files:**
- Modify: `internal/controller/sweeper.go`, `internal/controller/sweeper_test.go`

- [ ] **Step 1: Write the failing test.** Extend `TestSweeper_DeletesOrphanedChildrenAndRecord` with ctx logger `testlog.New(2)`:
  - The `orphaned instance: sweeping provider children` line is level 0 with `record`, `instanceUID`, `cluster` (record label `LabelConsumerCluster`) and `export` (record label `LabelBlueprint`).
  - Each `swept orphaned child` line is level 0 with `gvk` (token as recorded), `child` (was `name`), `instanceUID`, `cluster`.
  
  Extend `TestSweeper_SkipsFreshAndUnparseable`: the fresh record yields a level-2 `liveness record fresh` line with `record`, and the unparseable record still yields a level-0 line.

- [ ] **Step 2: Run** `go test ./internal/controller/ -run TestSweeper` → FAIL.

- [ ] **Step 3: Implement.** Move `starting orphan sweeper` to `V(1)`. The grace-period skip is already `V(1)`. Add `V(2)` "liveness record fresh". Rename `name`→`child` on the child line, and pass `cluster`/`export`/`instanceUID` into `sweepChildren`'s logger via `WithValues`. Keep unparseable records at default.

- [ ] **Step 4: Run** `go test ./internal/controller/ -run TestSweeper` → PASS.

- [ ] **Step 5: Stage and propose commit**

  ```bash
  git add internal/controller/sweeper.go internal/controller/sweeper_test.go
  ```
  Propose: `gitcs 'feat(sweeper): align log tiers and fields with ADR 0013 (#72)'`

---

### Task 6: Entrypoint wiring and operations docs

**Files:**
- Modify: `cmd/controller/main.go`, `docs/operations.md`

**Interfaces:**
- Consumes: `kropctrl.InstanceLogConstructor`, new `OnPublished` signature, `supervisor.Supervisor.Log`.

- [ ] **Step 1: Implement `main.go`**
  - `servedBlueprint` gains `name string` (RGD name). `OnPublished` stores `blueprint` into it.
  - Instance controller: `.WithLogConstructor(kropctrl.InstanceLogConstructor(imgr.GetLogger().WithValues("controller", "krop-instance-"+exportName), sb.name, exportName, sb.gvk.Kind))`.
  - In `reconcileFn`, drop the `l` logger and both `reconciled…` lines (the V(1) pass-end line replaces them).
  - Registrar controller: `.WithLogConstructor(func(req *reconcile.Request) logr.Logger { l := mgr.GetLogger().WithValues("controller", "krop-registrar"); if req != nil { l = l.WithValues("blueprint", req.Name) }; return l })`.
  - `exportLog := entryLog.WithValues("export", exportName)` (was `apiExport`).

- [ ] **Step 2: Build and vet**

  Run: `go build ./... && go vet ./...`
  Expected: no output, exit 0.

- [ ] **Step 3: Update `docs/operations.md` Observability → Logs bullet.** Cover the tier table from the spec, the field list, and the zap mapping (`info` default, `debug` = V(1), `2` = V(2), set via `controller.extraArgs`). Add two LogQL examples: one instance's history, `{app="krop-controller"} | json | blueprint="bucket" | namespace="team-a" | name="logs"`; and all pending passes of a blueprint, `{app="krop-controller"} | json | blueprint="bucket" | outcome="Pending"`. Note that `kubectl logs` shows the same JSON.

- [ ] **Step 4: Stage and propose commit**

  ```bash
  git add cmd/controller/main.go docs/operations.md
  ```
  Propose: `gitcs 'feat: tag instance and registrar logs with blueprint fields (#72)'`

---

### Task 7: Full verification

- [ ] **Step 1:** `make test`. Expected: all suites PASS, including envtest (`TestControllerIntegration`, registrar e2e).
- [ ] **Step 2:** `make lint`. Expected: 0 issues; license headers present on new files.
- [ ] **Step 3:** Smoke-check output by running `make run-controller` against a dev kcp, if one is available, at `--zap-log-level=2`. Confirm one `pass ended` line per pass with `blueprint/kind/cluster/namespace/name/instanceUID`, and no `reconciled; requeueing` at default. If no dev environment is available, say so explicitly in the PR instead of claiming it was checked.
- [ ] **Step 4:** Self-review `git diff main...` for unrelated changes and logged object content. Then propose the PR description: why (#72 diagnosis, provider observability, ADR 0013), the tier summary, and the follow-ups (includeWhen pending bugfix, #49).
