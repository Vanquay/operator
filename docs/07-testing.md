# 7. Testing operators

```sh
make test     # everything except e2e (~20s): unit + envtest + webhooks
make lint     # static checks
make test-e2e # full deploy into a throwaway kind cluster (needs Docker + kind)
```

## The test pyramid in this repo

| Layer | Files | Needs | Speed | Tests |
|---|---|---|---|---|
| Pure unit | `internal/schedule/schedule_test.go`, `internal/controller/webapp_resources_test.go` | nothing | ms | Logic: time windows, DST, hashing, builders keeping server defaults |
| Controller integration (envtest) | `internal/controller/*_controller_test.go`, `samples_test.go` | `bin/k8s/*` | seconds | Reconcile behaviour against a real API server |
| Webhook integration (envtest) | `internal/webhook/v1alpha1/*_test.go` | `bin/k8s/*` | seconds | Admission over HTTPS, CEL, defaulting |
| End-to-end | `test/e2e/` (kubebuilder scaffold) | kind + Docker | minutes | The built image actually deploys and serves metrics |

Push as much logic as possible **down** the pyramid. `internal/schedule` exists partly so its
tricky cases can be covered by fast unit tests.

## envtest: a real API server without a cluster

`suite_test.go` starts `etcd` and `kube-apiserver` binaries (downloaded by `make setup-envtest`
into `bin/k8s/`), installs the CRDs from `config/crd/bases`, and gives you a `*rest.Config`.

What you get:
- real CRD schema validation, defaults and **CEL rules** (`is rejected by the API server's CEL
  rules…`),
- real optimistic concurrency, resourceVersions, watches, subresources (`/status`, `/scale`).

What you **don't** get:
- **No kube-controller-manager.** Deployments never create Pods and never update their
  status. Tests that need "ready pods" write Deployment status themselves (*reports Available
  once the Deployment's pods are ready*).
- **No garbage collector.** Deleting a WebApp doesn't delete its Deployment. Test the owner
  reference instead (`metav1.IsControlledBy(dep, app)`).
- **No RBAC enforcement.** The test client is a superuser. Missing `+kubebuilder:rbac` markers
  only show up in a real cluster (e2e).

## Two styles of controller test

**1. Run the real manager** (WebApp tests). The manager starts in `BeforeSuite` with the same
wiring as `cmd/main.go`. Tests create objects with `k8sClient` and **wait** for the controller:

```go
Eventually(func(g Gomega) {
    cond := conditionOf(g, "with-secret", platformv1alpha1.ConditionDegraded)
    g.Expect(cond).NotTo(BeNil())
    g.Expect(cond.Reason).To(Equal(platformv1alpha1.ReasonSecretNotFound))
}).Should(Succeed())
```

This tests watches, owner-reference routing, field indexes and the cache: the parts that break
in real life. `Eventually` retries the function until it passes (timeout 15s, set in the suite).
Pass `g Gomega` into the function and use `g.Expect` inside it, so a failed attempt retries
instead of failing the test.

`Consistently` is the opposite: the assertion must hold for the whole period. It's used to prove
that something does **not** happen (no Deployment rewrite; image unchanged while paused).

**2. Call `Reconcile()` directly** (ScalingSchedule tests). With a fake clock and a fake event
recorder, every assertion runs right after the call, with no waiting and full determinism:

```go
result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key("shop-hours")})
Expect(result.RequeueAfter).To(Equal(30*time.Minute + transitionSlack))
Expect(recorder.Events).To(Receive(ContainSubstring("Scaled")))
```

Use this when time or ordering matters. Use style 1 when you need to prove the wiring works.

## Ginkgo and Gomega in 60 seconds

```go
var _ = Describe("WebApp Controller", func() {   // a group
    It("creates a Deployment", func() {           // a test case
        By("creating the WebApp")                 // a labelled step in the output
        Expect(k8sClient.Create(ctx, app)).To(Succeed())
    })
})
```

- `Expect(x).To(Equal(y))`, `.NotTo(...)`, `HaveKey`, `ContainElement`, `ContainSubstring`,
  `BeTemporally("==", t)`, `HaveValue(...)` (dereferences pointers), `MatchError(...)`.
- `DescribeTable` / `Entry` is Ginkgo's table-driven test (see `imageIsPinned`).
- `GinkgoHelper()` is Ginkgo's `t.Helper()`.
- Focus one spec: `go test ./internal/controller/ -ginkgo.focus="Secret" -ginkgo.v`.

## Useful commands

```sh
# One plain-Go test, no envtest:
go test ./internal/schedule/ -run TestEvaluateAcrossDaylightSaving -v

# envtest suites outside make (KUBEBUILDER_ASSETS must be an absolute path):
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37 --bin-dir "$PWD/bin" -p path)"
go test ./internal/controller/ -ginkgo.v

# Coverage report:
make test && go tool cover -html=cover.out
```

IDEs (VS Code, GoLand) can run the envtest suites directly: `getFirstFoundEnvTestBinaryDir()`
in the suite finds `bin/k8s/...` once `make setup-envtest` has run.

## Writing tests that aren't flaky

- **Unique names per test.** Specs share one API server, so don't reuse names.
- **Retry read-modify-write** in tests too (`updateWebApp` wraps the update in `Eventually`),
  because the controller may update the same object at the same moment.
- **Never `time.Sleep`.** Use `Eventually`/`Consistently`, or a fake clock.
- **Production code should handle conflicts** (`retry.RetryOnConflict` in `scaleTarget`). Flaky
  tests are often the first sign of a real race.

Next: [8. Running locally and deploying](08-running-locally.md)
