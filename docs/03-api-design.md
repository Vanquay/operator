# 3. Designing the API (CRDs)

Your CRD is a **public API**. Users write YAML against it, other tools parse it, and once it
ships you can't casually change it. Spend more time here than anywhere else.

Files: `api/v1alpha1/webapp_types.go`, `api/v1alpha1/scalingschedule_types.go`,
`api/v1alpha1/groupversion_info.go`.

## From Go struct to CRD

```
api/v1alpha1/*_types.go ──make manifests──► config/crd/bases/platform.example.com_webapps.yaml
        (structs + markers)      (controller-gen)        (OpenAPI v3 schema the API server enforces)
                         ──make generate───► api/v1alpha1/zz_generated.deepcopy.go
```

Open `config/crd/bases/platform.example.com_webapps.yaml` next to `webapp_types.go`. Every
field, comment and marker maps to something in the YAML. Your doc comments become
`kubectl explain` output:

```sh
kubectl explain webapp.spec.secretRef
```

## Group, version, kind

- **Group** `platform.example.com` (from `--domain example.com --group platform`). Use a domain
  you own in real projects.
- **Version** `v1alpha1`. Kubernetes versions signal stability: `v1alpha1` → `v1beta1` → `v1`.
  Alpha means "we may break this". To move to `v1beta1` later, you add a second version and a
  **conversion webhook** (`kubebuilder create webhook --conversion`).
- **Kind** `WebApp` (singular, CamelCase). The resource (URL) name is `webapps`.

## Validation, cheapest first

| Layer | Example in this repo | Runs where | Use for |
|---|---|---|---|
| Go types | `int32`, `bool`, `map[string]string` | API server (schema) | Shape |
| OpenAPI markers | `+kubebuilder:validation:Minimum=0`, `Pattern`, `MaxItems`, `Enum` | API server | Single-field rules |
| Defaults | `+kubebuilder:default=8080` | API server | Simple constant defaults |
| **CEL rules** | `+kubebuilder:validation:XValidation:rule="..."` | API server | Cross-field rules, immutability, list checks |
| Webhooks | `internal/webhook/v1alpha1` | Your operator, over HTTPS | Anything needing Go code or other objects |

Rules higher in the table can't be down or slow, because they live in the API server. **Prefer
them.** Reach for a webhook only when you must.

### CEL examples in this repo

```go
// Reserved env var: a list rule
// +kubebuilder:validation:XValidation:rule="!has(self.env) || !self.env.exists(e, e.name == 'PORT')",message="env var PORT is reserved; set spec.port instead"

// Valid ConfigMap keys: a map rule
// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[-._a-zA-Z0-9]+$'))",message="..."

// Immutability: a transition rule using oldSelf
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetRef is immutable"

// Cross-field rule on a list item
// +kubebuilder:validation:XValidation:rule="self.start != self.end",message="start and end must differ"
```

Try them against a cluster:

```sh
kubectl apply -f - <<'EOF'
apiVersion: platform.example.com/v1alpha1
kind: WebApp
metadata: {name: bad}
spec:
  image: nginx:1.27
  env: [{name: PORT, value: "1"}]
EOF
# The WebApp "bad" is invalid: spec: Invalid value: env var PORT is reserved; set spec.port instead
```

## Optional vs required, pointers vs values

- Mark every field `+required` or `+optional`. Don't rely on `omitempty` to imply it.
- Optional numbers and bools that need to tell "unset" from zero use **pointers**
  (`Replicas *int32`). If zero is never meaningful (`Port`), a value with a default is fine.
- Optional sub-objects whose *presence* means something use pointers: `Ingress *IngressSpec`.
  `nil` means "no Ingress", and the controller deletes one if it exists.

## Lists that behave well with `kubectl apply`

```go
// +listType=map
// +listMapKey=name
Env []corev1.EnvVar `json:"env,omitempty"`
```

`listType=map` tells server-side apply that items are identified by `name`, so two tools can
each own different entries. Other values: `set` (unique scalars, used for `days`) and `atomic`
(replace the whole list). Status `conditions` use `map` keyed by `type`.

## Status design

```go
type WebAppStatus struct {
    ObservedGeneration int64              // which spec generation this status describes
    Replicas, ReadyReplicas int32         // raw observations
    Selector string                       // needed by the /scale subresource
    URL string                            // useful derived info
    ConfigHash string                     // what config revision is rolled out
    Conditions []metav1.Condition         // the health summary
}
```

**Conditions over phases.** A single `phase: Running` string can't express "available but a
rollout is in progress and a Secret is missing". Conditions can:

| Type | True means | Reasons used |
|---|---|---|
| `Available` | Pods are serving | `ReplicasAvailable` / `ReplicasNotReady`, `ScaledToZero` |
| `Progressing` | A rollout is in flight | `RollingOut` / `RolloutComplete`, `Paused` |
| `Degraded` | The operator can't converge | `SecretNotFound`, `ReconcileFailed` / `AsExpected` |

They're machine-readable (`kubectl wait --for=condition=Available webapp/storefront`) and each has
its own `lastTransitionTime`.

## Subresources

```go
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
```

- **`/status`** is a separate endpoint. Updates to the main object ignore `status`, and
  updates to `/status` ignore everything else. Users and the controller can't clobber each
  other. RBAC can grant them separately (`webapps/status`).
- **`/scale`** exposes the generic `autoscaling/v1 Scale` view of the object. You get
  `kubectl scale webapp storefront --replicas=5` for free, the HorizontalPodAutoscaler can target
  WebApps, and our ScalingSchedule uses it with a narrow `webapps/scale` permission.

## Printer columns, short names and categories

```go
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1
// +kubebuilder:resource:shortName=wa,categories=platform
```

- Columns shape `kubectl get` output. `priority=1` columns only show with `-o wide`.
- `shortName=wa` enables `kubectl get wa`.
- `categories=platform` enables `kubectl get platform`, which lists WebApps and
  ScalingSchedules together.

## Evolving the API safely

- **Adding** an optional field is safe.
- **Removing or renaming** a field, or **tightening** validation, breaks existing objects and
  clients. Do it in a new version with conversion.
- Defaults are applied when objects are read from etcd too, so a new default affects old
  objects on their next write.
- Always run `make manifests generate` after editing types. CI should fail if generated files
  are stale (`git diff --exit-code` after regenerating).

Next: [4. The WebApp controller](04-webapp-controller.md)
