# 1. Operators 101

## The one idea everything rests on

Kubernetes is a database of **desired state** (etcd behind the API server) plus a set of
**controllers**. Each controller is a loop that says:

> Read what the user *wants*. Look at what *is*. Change the world a little so the two match. Repeat.

The Deployment controller turns a `Deployment` into `ReplicaSet`s. The ReplicaSet controller
turns those into `Pod`s. The kubelet turns Pods into running containers. Nobody calls these
controllers directly. They watch the API server and react.

An **operator** is a controller you write for **your own resource types**. You teach Kubernetes
a new noun (`WebApp`) by defining a **CustomResourceDefinition (CRD)**. Then you run a controller
that knows how to make that noun real.

```
                   ┌──────────── you write this ────────────┐
kubectl apply  →   │ WebApp (CRD)  →  WebApp controller      │  →  Deployment, Service, …
                   └─────────────────────────────────────────┘
                                                  ↓ built-in controllers take it from there
                                               ReplicaSet → Pods → containers
```

## The reconcile loop

The heart of an operator is one function:

```go
func (r *WebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
```

`req` contains only a **namespace and name**. There is no "what changed" information. That is
on purpose, and it gives you three properties you must respect:

1. **Level-triggered, not edge-triggered.** Don't write "if the image changed, then…". Always
   compute the full desired state from the current spec and converge on it. If events are
   dropped, duplicated, or merged, the result is still correct.
2. **Idempotent.** Running `Reconcile` ten times in a row must be the same as running it once.
   Every write in this repo either checks before writing (`CreateOrUpdate`, `patchStatus`
   comparing old and new) or is naturally idempotent.
3. **Stateless between calls.** Anything you need to remember goes into the object's
   `status` (or the cluster), not into a Go variable. The process can restart at any time.

What you return controls what happens next:

| Return | Meaning | Used in this repo for |
|---|---|---|
| `ctrl.Result{}, nil` | Done. Wake me when a watched object changes. | Normal success |
| `ctrl.Result{}, err` | Failed. Retry with exponential backoff (5 ms → … → ~16 min). | API errors, catalog write failures |
| `ctrl.Result{RequeueAfter: d}, nil` | Done for now, but check again in `d`. | ScalingSchedule waiting for 18:00 |

## What happens between `kubectl apply` and `Reconcile`

```
kubectl apply ──► kube-apiserver ──► admission webhooks ──► etcd
                        │
                        │ watch stream (HTTP long-poll)
                        ▼
                ┌─ Informer ────────────────┐
                │ keeps a local cache of    │──► event handlers ──► workqueue ──► Reconcile()
                │ every watched object      │      (For/Owns/Watches)   (dedupes,     (N workers)
                └───────────────────────────┘                           rate-limits)
```

- **Informer / cache.** controller-runtime opens one watch per resource type and keeps an
  in-memory copy of every object. `r.Get` and `r.List` read from this cache, so they are cheap.
  The catch: the cache can be a few milliseconds **stale**. That's why writes can fail with
  `409 Conflict`, and why a few places in this repo deliberately use the uncached
  `mgr.GetAPIReader()`.
- **Event handlers.** These turn "a Deployment changed" into "reconcile WebApp default/shop".
  You configure them in `SetupWithManager` with `For`, `Owns` and `Watches`.
- **Workqueue.** It deduplicates: ten events for the same WebApp while one reconcile is running
  become one more reconcile. It also guarantees that the **same key is never reconciled by two
  workers at once**. Different keys can run in parallel (`MaxConcurrentReconciles: 2` in the
  WebApp controller).

## The manager

`cmd/main.go` builds a **manager**. It is the process-level container that holds:

- the shared cache and client used by all controllers,
- the controllers themselves,
- the webhook HTTPS server,
- the `/metrics` endpoint and the `/healthz` and `/readyz` probes,
- **leader election.** You can run several replicas of the operator for availability, but only
  the elected leader runs controllers. The others wait. Without it, two copies would fight.

## Spec vs status

Every well-designed resource splits into:

- **`spec`**: what the user wants. Only users (and other controllers acting for them) write it.
- **`status`**: what the controller observed. Only the controller writes it, through the
  separate `/status` subresource, so the two never overwrite each other.

`metadata.generation` increases every time `spec` changes. The controller copies the generation
it acted on into `status.observedGeneration`. If `observedGeneration < generation`, the status
is out of date. Tools like `kubectl wait` and Argo CD rely on this.

## Ownership and garbage collection

When the WebApp controller creates a Deployment, it sets an **owner reference** on it:

```yaml
metadata:
  ownerReferences:
  - apiVersion: platform.example.com/v1alpha1
    kind: WebApp
    name: storefront
    controller: true
    blockOwnerDeletion: true
```

That one field does two jobs:

1. **Event routing.** `Owns(&appsv1.Deployment{})` means "when a Deployment changes, look at its
   controller owner reference and reconcile *that* WebApp".
2. **Garbage collection.** When the WebApp is deleted, kube-controller-manager's garbage
   collector deletes everything it owns. You write no cleanup code.

Owner references only work **within one namespace** (or from namespaced to cluster-scoped). For
anything else (cross-namespace objects, cloud resources, DNS records) you need a **finalizer**.
See [04-webapp-controller.md](04-webapp-controller.md#finalizers).

## Vocabulary cheat sheet

| Term | Meaning |
|---|---|
| GVK | Group/Version/Kind, e.g. `platform.example.com/v1alpha1, Kind=WebApp` |
| GVR | Group/Version/Resource, e.g. `.../v1alpha1, Resource=webapps` (the URL form) |
| Scheme | Registry mapping Go types ↔ GVKs. `AddToScheme` in `api/v1alpha1/groupversion_info.go`. |
| Subresource | A separate endpoint on an object: `/status`, `/scale` |
| Condition | A typed status entry, e.g. `Available=True`. See `metav1.Condition`. |
| Finalizer | A string in `metadata.finalizers` that blocks deletion until a controller removes it |
| Admission webhook | An HTTPS callback the API server makes before storing an object |
| envtest | Real `kube-apiserver` + `etcd` binaries started by your tests, without nodes |

Next: [2. Go concepts used in this repo](02-go-concepts.md)
