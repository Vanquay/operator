# 2. Go concepts used in this repo

This is a primer for the Go you'll meet in this codebase, each idea tied to a real file. It's no
substitute for [A Tour of Go](https://go.dev/tour/), which takes about 2 hours and is worth doing
first, but it shows *why* operator code looks the way it does.

---

## Modules, packages and visibility

- `go.mod` declares the **module path** `github.com/example/platform-operator`. Every import of
  our own code starts with it: `"github.com/example/platform-operator/internal/catalog"`.
- A **package** is a directory. All `.go` files in `internal/schedule/` are `package schedule`.
- **Capitalised names are exported** (public): `schedule.FromSpec`, `catalog.Entry`. Lower-case
  names (`computeConfigHash`, `appContainer`) are visible only inside their package. There are no
  `public`/`private` keywords.
- Anything under an **`internal/`** directory can only be imported by code inside this module.
  The compiler enforces it, which keeps the public surface small.
- Import aliases: `platformv1alpha1 "github.com/.../api/v1alpha1"`. Kubernetes code aliases API
  packages by group+version (`appsv1`, `corev1`, `metav1`) because many are just named `v1`.

## Structs and struct tags

```go
// api/v1alpha1/webapp_types.go
type WebAppSpec struct {
    Image    string `json:"image"`
    Replicas *int32 `json:"replicas,omitempty"`
}
```

- The back-quoted text is a **struct tag**: metadata that libraries read with reflection.
  `encoding/json` uses `json:"image"` to name the field in JSON/YAML.
- `omitempty` drops the field when it is a zero value. `omitzero` (Go 1.24+) also handles
  structs, which `omitempty` never omits.
- The `// +kubebuilder:validation:Minimum=0` lines are ordinary **comments** to Go. Only
  `controller-gen` reads them.

## Pointers

Go passes everything **by value** (as a copy). A pointer (`*T`) lets you share and modify.

**Optional fields.** `Replicas *int32` can be `nil` ("not set") or point to `0` ("scale to
zero"). A plain `int32` can't tell those apart. That's why optional numeric API fields are
pointers.

**Making a pointer to a value.** Since Go 1.26 `new` accepts a value:

```go
dep.Spec.Replicas = new(desiredReplicas(app))  // internal/controller/webapp_resources.go
```

Older code writes `ptr.To(x)` from `k8s.io/utils/ptr`. You'll see both in the wild.

**Pointer receivers.** `func (r *WebAppReconciler) Reconcile(...)` has a pointer receiver, so
`r` is not copied on every call and methods could modify it. Kubernetes objects are almost always
passed as pointers (`*platformv1alpha1.WebApp`), because the client fills them in place:
`r.Get(ctx, key, app)` writes into `app`.

**The range-loop gotcha.** In `internal/controller/webapp_resources.go`:

```go
for i := range spec.Containers {          // index...
    if spec.Containers[i].Name == appContainerName {
        return &spec.Containers[i]        // ...so this points INTO the slice
    }
}
```

`for _, c := range spec.Containers` would give you a *copy* of each container, so changes to `c`
would be lost.

## Interfaces are implicit

```go
// internal/catalog/catalog.go
type Catalog interface {
    Register(ctx context.Context, e Entry) error
    Deregister(ctx context.Context, namespace, name string) error
}
```

Any type with those two methods *is* a `Catalog`. You never write "implements". The WebApp
controller depends only on the interface, so you could swap in an HTTP-backed registry or a test
fake without touching it.

A compile-time check that a type satisfies an interface:

```go
var _ Catalog = (*ConfigMapCatalog)(nil)
```

**Accept small interfaces.** `ScalingScheduleValidator` only needs to read, so its field is
`client.Reader` (Get + List), not the full `client.Client`. The test passes `k8sClient`
and main passes `mgr.GetAPIReader()`. Both satisfy `client.Reader`.

## Embedding

```go
type WebAppReconciler struct {
    client.Client          // embedded: no field name
    Scheme *runtime.Scheme
}
```

The embedded interface's methods are **promoted**, so you write `r.Get(...)`, `r.Update(...)`.
The same trick is why `app.Name` works on a WebApp: `WebApp` embeds `metav1.ObjectMeta`, which
has a `Name` field. Embedding is composition, not inheritance.

## Errors are values

Go has no exceptions. Functions return an `error` as their last result and callers check it:

```go
if err := r.Update(ctx, app); err != nil {
    return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
}
```

- **Wrap** with `fmt.Errorf("context: %w", err)`. The `%w` keeps the original error inside, so
  callers can still inspect it.
- **Inspect** with `errors.Is` / `errors.As`, or the Kubernetes helpers built on them:
  `apierrors.IsNotFound(err)`, `apierrors.IsConflict(err)`. They see through wrapping.
- `client.IgnoreNotFound(err)` returns `nil` for NotFound, else `err`. It's very common at the
  top of `Reconcile`.
- `field.ErrorList` (in `internal/schedule/spec.go`) is the Kubernetes way to collect *several*
  validation errors with exact field paths like `spec.windows[1].end`.

## `context.Context`

Almost every call takes `ctx` as its first argument. A context carries:

- **cancellation.** When the manager shuts down, `ctx` is cancelled and in-flight API calls
  return promptly,
- **deadlines**, and
- **request-scoped values.** controller-runtime puts a logger with the WebApp's name in it,
  which is why `logf.FromContext(ctx)` gives you logs already tagged with
  `"WebApp": {"name": "...", "namespace": "..."}`.

Always pass the `ctx` you were given; don't create `context.Background()` in the middle of a
reconcile.

## Closures (functions as values)

`ensure()` in `webapp_controller.go` takes a `mutate func()`. Callers pass a function literal that
**captures** local variables:

```go
dep := &appsv1.Deployment{ObjectMeta: childMeta(app)}
r.ensure(ctx, app, dep, func() { mutateDeployment(dep, app, configHash) })
```

`retry.RetryOnConflict(backoff, func() error {...})` in the catalog uses the same idea: "here is
the code to retry".

## Maps and slices

- **Map iteration order is random by design.** `computeConfigHash` sorts keys first
  (`slices.Sorted(maps.Keys(config))`). Without that, the hash would change between runs and
  restart pods for nothing.
- `maps.Keys` returns an **iterator** (Go 1.23+). `slices.Sorted` collects and sorts it.
- `maps.Clone`, `maps.Copy` and `slices.Contains` are in the standard library. The linter
  forbids the old `sort` package in favour of `slices`.
- A `nil` map can be read but **not written**. Writing panics. That's why the code does
  `if cm.Data == nil { cm.Data = map[string]string{} }` before assigning.

## Generics

You'll mostly *use* generics rather than write them:

- `ctrl.NewWebhookManagedBy(mgr, &platformv1alpha1.WebApp{})` is generic over the object type,
  so `WebAppValidator.ValidateCreate` receives a typed `*platformv1alpha1.WebApp` instead of a
  `runtime.Object` you'd have to type-assert.
- `slices.SortFunc(candidates, time.Time.Compare)` is generic over the element type. It also
  shows a **method expression**: `time.Time.Compare` is a plain function `func(a, b time.Time) int`.

## Type assertions

```go
app := obj.(*platformv1alpha1.WebApp)   // in the IndexField func
```

`obj` is the interface `client.Object`, and `.(*WebApp)` asserts the concrete type. It panics if
wrong, which is fine here because the index is registered for WebApps only. The safe form is
`app, ok := obj.(*WebApp)`.

## Functional options and variadic parameters

Test helpers use `opts ...func(*WebApp)`:

```go
newWebApp("hello", func(a *platformv1alpha1.WebApp) { a.Spec.Replicas = new(int32(2)) })
```

`...` makes the parameter variadic, so callers pass zero or more functions.

## `init()` and blank imports

- `func init()` runs automatically when a package is loaded. `internal/metrics/metrics.go` uses
  it to register Prometheus metrics. The `api/v1alpha1` types use it to register with the scheme.
- `import _ "time/tzdata"` in `cmd/main.go` imports a package **only for its side effect** (its
  `init` embeds the time zone database). `_` means "I won't use any names from it".

## Concurrency

You won't write many goroutines in an operator, but it's highly concurrent:

- The manager runs every controller, the cache and the webhook server in their own goroutines.
- `MaxConcurrentReconciles: 2` means two `Reconcile` calls (for *different* WebApps) can run at
  once. So the reconciler struct must not hold per-request state in fields. Use local variables.
- The catalog ConfigMap is shared by all WebApps, so concurrent writes are expected. Kubernetes'
  optimistic concurrency (resourceVersion + 409 Conflict) plus a retry loop replace a mutex.
- Tests start the manager with `go func() { ... mgr.Start(ctx) }()`.

## Time

- `time.Time` is an instant; `time.Duration` is a length (`9*time.Hour`).
- Compare instants with `t1.Equal(t2)`, not `==` (location and monotonic clock data differ).
- `time.Date(y, m, d+1, 8, 0, 0, 0, loc)` **normalises** overflow (day 32 → next month) and
  handles daylight saving, which is why `internal/schedule` builds boundaries this way instead of
  adding 24h. `TestEvaluateAcrossDaylightSaving` proves it.
- Don't call `time.Now()` in logic you want to test. The ScalingSchedule reconciler takes a
  `clock.PassiveClock` so tests can set the time.

## Testing idioms

- **Table-driven tests**: a slice of cases plus `t.Run(tc.name, ...)`. See
  `internal/schedule/schedule_test.go`.
- `t.Helper()` makes failures report the caller's line.
- `go test ./internal/schedule/ -run TestOverlaps -v` runs one test verbosely.
- Ginkgo/Gomega (BDD style, used by kubebuilder) is covered in [07-testing.md](07-testing.md).

## Tooling

| Command | Purpose |
|---|---|
| `go build ./...` | Compile everything |
| `go vet ./...` | Catch suspicious code (wrong printf args, copying locks, …) |
| `go fmt ./...` / `gofmt -l .` | The one true formatting. Never argue about style. |
| `go doc sigs.k8s.io/controller-runtime/pkg/client Client` | Read API docs in the terminal |
| `go mod tidy` | Sync `go.mod`/`go.sum` with imports |
| `make lint` | golangci-lint with this repo's rules (`.golangci.yml`) |

Next: [3. Designing the API](03-api-design.md)
