# 9. Production checklist

What makes an operator safe to run in a real cluster. Each item notes where this repo already
handles it, and what you'd still add before betting a business on it.

## Already in place

### Correctness
- [x] **Level-triggered, idempotent reconcile.** It recomputes the full desired state every time
  (`Reconcile`).
- [x] **No-op writes are skipped** (`CreateOrUpdate` diffing, `patchStatus` comparing). This
  avoids hot loops and API load.
- [x] **Mutate functions don't fight server defaults.** Tested by *does not rewrite its children
  when nothing changed*.
- [x] **Never adopts objects it didn't create** (`ensure()` ownership check).
- [x] **Optimistic concurrency handled.** `RetryOnConflict` on shared or hot objects (catalog,
  `/scale`); other conflicts back off and retry.
- [x] **Finalizer for external cleanup.** Added before the external write, idempotent, and
  tolerant of the target already being gone.
- [x] **Immutable fields respected** (Deployment selector set only on create; `targetRef` CEL
  rule).
- [x] **The controller re-validates.** It doesn't trust that webhooks ran (`schedule.FromSpec`
  in the reconciler).
- [x] **Time zone data embedded** (`_ "time/tzdata"`), so the distroless image works anywhere.

### API
- [x] Spec/status split with the `/status` subresource; `observedGeneration`; standard
  `metav1.Condition`s.
- [x] Validation layered from cheapest to most expensive: schema → CEL → webhook.
- [x] `/scale` subresource, so the HPA, `kubectl scale` and the ScalingSchedule all work.
- [x] Printer columns, short names and categories for a good `kubectl` experience.

### Security
- [x] **Least-privilege RBAC** generated from markers. No `create`/`delete` on WebApps;
  read-only on Secrets.
- [x] Secrets watched **metadata-only**, so secret values never sit in the operator's memory.
- [x] Pod hardening in `config/manager/manager.yaml`: `runAsNonRoot`, `readOnlyRootFilesystem`,
  `capabilities: drop: [ALL]`, `seccompProfile: RuntimeDefault`, distroless `nonroot` image.
- [x] Metrics served over HTTPS behind Kubernetes authn/authz (`filters.WithAuthenticationAndAuthorization`).
- [x] HTTP/2 disabled by default (Rapid Reset CVEs), `--enable-http2` to opt in.
- [x] Webhook TLS from cert-manager.

### Operability
- [x] **Leader election** (`--leader-elect`), so extra replicas are hot standbys.
- [x] Liveness and readiness probes (`/healthz`, `/readyz`).
- [x] Requests and limits on the manager pod.
- [x] Kubernetes **Events** for user-visible happenings; **structured logs** checked by
  `logcheck`.
- [x] **Prometheus metrics**: controller-runtime's built-ins (`controller_runtime_reconcile_total`,
  `..._errors_total`, `workqueue_depth`, `..._reconcile_time_seconds`) plus domain gauges and
  counters.
- [x] Graceful shutdown on SIGTERM (`ctrl.SetupSignalHandler()`).

### Engineering
- [x] Three test layers plus the scaffolded e2e; samples tested against the schema.
- [x] golangci-lint with Kubernetes conventions; CI workflows in `.github/workflows/`.
- [x] Generated code never hand-edited; `make manifests generate` is the source of truth.

## Before real production

### Availability
- [ ] **`replicas: 2` + a PodDisruptionBudget for the operator.** With `failurePolicy: Fail`,
  a single operator pod being rescheduled blocks every WebApp write for that time.
- [ ] Set webhook `timeoutSeconds` (default 10s) lower, and add a `namespaceSelector` so the
  webhook can never block `kube-system`.
- [ ] Spread replicas across nodes/zones (topology spread constraints).

### Scale
- [ ] **Scope the cache.** Today it watches ConfigMaps, Services, Deployments, Ingresses and
  Secret metadata cluster-wide. On large clusters, filter by label (`cache.Options.ByObject`) or
  namespace, and size the memory limit (128Mi is a starting point, not a guarantee).
- [ ] The catalog is one ConfigMap, capped at **1 MiB** (about 5,000 entries). At scale, use a
  CRD per entry or an external registry.
- [ ] Tune `MaxConcurrentReconciles` and the client QPS/burst (`ctrl.GetConfigOrDie()` → `rest.Config.QPS`).

### Safety and API lifecycle
- [ ] Use a domain you own for the API group, and a real module path.
- [ ] Plan versioning: `v1alpha1` → `v1beta1` with a **conversion webhook**, and storage version
  migration.
- [ ] Detect duplicate ScalingSchedules in the controller too (webhooks are best-effort).
- [ ] Consider **server-side apply** for children, for clean field ownership with other tools
  (Argo CD, Helm, HPA).

### Observability
- [ ] Alerts: reconcile error rate, `workqueue_depth` growing, `workqueue_unfinished_work_seconds`,
  WebApps with `Degraded=True` for more than N minutes, webhook latency (API server metrics).
- [ ] Dashboards for the custom metrics.
- [ ] Optionally, tracing (OpenTelemetry) around external calls.

### Supply chain and releases
- [ ] Versioned, signed images (cosign), an SBOM, vulnerability scanning.
- [ ] Release artefacts: `make build-installer` → `dist/install.yaml`, or a Helm chart
  (`kubebuilder edit --plugins=helm/v2-alpha`).
- [ ] e2e in CI against the oldest and newest Kubernetes versions you support.
- [ ] An upgrade test: old operator + objects → new operator → objects still converge.

Next: [10. Exercises](10-exercises.md)
