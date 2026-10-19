# 6. Admission webhooks

Files: `internal/webhook/v1alpha1/webapp_webhook.go`, `scalingschedule_webhook.go`,
generated `config/webhook/manifests.yaml`.

## Where webhooks sit

```
kubectl apply
   │
   ▼
kube-apiserver ── authn/authz
   │
   ├─► mutating admission ──► POST https://webhook-service/mutate-...-webapp     (Default)
   ├─► schema validation, +kubebuilder:default, CEL rules
   ├─► validating admission ─► POST https://webhook-service/validate-...-webapp  (ValidateCreate/Update)
   ▼
 etcd
```

The API server calls your operator over **HTTPS**, waits for the answer, then accepts or rejects
the request. That has consequences:

- **Webhooks need TLS certificates** the API server trusts. In-cluster, cert-manager issues them
  and injects the CA into the webhook configuration (`config/certmanager`, enabled in
  `config/default/kustomization.yaml`). envtest generates its own for tests.
- **If your webhook is down, writes fail.** The markers say `failurePolicy=fail`. That's correct
  for validation that guards correctness, but it means a crashing operator blocks
  `kubectl apply` for WebApps. Run 2+ replicas in production and keep webhooks fast.
- **Webhooks must not have side effects** (`sideEffects=None`). They may be called for dry-runs
  or for requests that a later webhook rejects.

## When to use a webhook vs the CRD

| Need | Use |
|---|---|
| Constant default (`replicas: 1`) | `+kubebuilder:default` |
| Range, pattern, enum, length | OpenAPI markers |
| Cross-field rule, immutability | CEL `XValidation` |
| Conditional default ("requests only if no limits") | **Defaulting webhook** |
| Logic that needs Go (`time.LoadLocation`, resource quantity maths) | **Validating webhook** (CEL can do some of this) |
| Rules that read *other* objects ("one schedule per WebApp") | **Validating webhook** |
| Soft advice ("pin your image tag") | **Warnings** from a validating webhook |

## The three webhooks here

### WebApp defaulting: `WebAppDefaulter.Default`

Fills in `requests: {cpu: 50m, memory: 64Mi}` **only if** the user set neither requests nor
limits. If they set limits, Kubernetes already defaults requests to the limits, and adding ours
could produce requests > limits. A good default never makes a valid object invalid.

### WebApp validation: `validateWebApp`

- **Error**: `requests` greater than `limits`. Kubernetes would reject the *Pod* anyway, but only
  later and inside a ReplicaSet event no one reads. Catching it at `kubectl apply` gives a clear
  message right away:

  ```
  The WebApp "web" is invalid: spec.resources.requests[memory]: Invalid value: "1Gi": must be less than or equal to the memory limit (512Mi)
  ```

- **Warning**: an unpinned image (`nginx`, `nginx:latest`). Warnings don't reject anything.
  kubectl prints them in yellow:

  ```
  Warning: spec.image "nginx:latest" uses an implicit or :latest tag; rollouts will not be reproducible. ...
  ```

  Warnings are a great way to nudge users towards best practice without breaking them.

### ScalingSchedule validation: `ScalingScheduleValidator.validate`

1. `schedule.FromSpec`: real time zone? valid `HH:MM`? The same function the controller uses.
2. **Warn** on overlapping windows (legal, since the first one wins, but often a mistake).
3. **Reject** a second schedule for the same WebApp. This needs to `List` other objects, so the
   validator has a `Client client.Reader` field, injected in `SetupScalingScheduleWebhookWithManager`.
4. **Warn** if the target WebApp doesn't exist yet. Don't reject it: with
   `kubectl apply -f dir/` the schedule may arrive before its WebApp.

It reads through `mgr.GetAPIReader()` (uncached) rather than the cache, so the decision rests on
what is stored *now*. Even so, cross-object checks in webhooks are **best-effort**: two
simultaneous creates can both pass. The controller must still cope.

## Errors in Kubernetes format

```go
return warnings, apierrors.NewInvalid(gk, ss.Name, field.ErrorList{
    field.Forbidden(field.NewPath("spec", "targetRef", "name"), "..."),
})
```

`field.ErrorList` plus `apierrors.NewInvalid` gives users a `422` response with precise field
paths, the same format built-in resources use.

## The generated configuration

The `// +kubebuilder:webhook:path=...,mutating=true,failurePolicy=fail,...,verbs=create;update`
markers become `config/webhook/manifests.yaml`. To also validate deletes, add `delete` to
`verbs` and implement `ValidateDelete`.

## Running without webhooks

`make run` starts the operator on your laptop, but the in-cluster API server can't reach
`localhost:9443` with valid TLS. Either:

```sh
ENABLE_WEBHOOKS=false make run      # skip webhook registration (cmd/main.go checks this)
```

or deploy into the cluster with cert-manager (`make dev-cluster dev-deploy`). Without webhooks,
CRD validation and CEL still apply, and the controllers re-validate schedules themselves.
That is why `ScalingScheduleReconciler` calls `schedule.FromSpec` instead of trusting the webhook.

Next: [7. Testing operators](07-testing.md)
