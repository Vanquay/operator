# 10. Exercises

The best way to learn this codebase is to change it. They're ordered roughly by difficulty. For
each: write the test first, then make it pass with `make test`.

General workflow after editing types or markers:

```sh
make manifests generate   # regenerate CRDs, RBAC, DeepCopy
make test lint
```

---

### 1. Add a printer column (warm-up)
Show `status.configHash` in `kubectl get webapps -o wide`.
*Hint:* one `+kubebuilder:printcolumn` marker with `priority=1`. Check the generated
`additionalPrinterColumns` in the CRD YAML.

### 2. Protect important apps from deletion
Reject `kubectl delete` of a WebApp annotated `platform.example.com/protected: "true"`.
*Hints:* add `delete` to the validating webhook marker's `verbs`, then implement
`WebAppValidator.ValidateDelete`. Test it through the API server in `webapp_webhook_test.go`.

### 3. A PodDisruptionBudget child
When `replicas >= 2`, create a `policy/v1` PodDisruptionBudget with `maxUnavailable: 1`.
Delete it when replicas drop below 2.
*Hints:* copy the pattern of `reconcileIngress` (a conditional child). Add `Owns(&policyv1.PodDisruptionBudget{})`
and an RBAC marker for `policy`/`poddisruptionbudgets`. What happens if you forget the marker?
(envtest won't tell you. Why not?)

### 4. A new condition from a child's status
Add an `IngressReady` condition that becomes True once the Ingress has
`status.loadBalancer.ingress` populated. In tests, write that Ingress status yourself, just like
the Deployment status test does.

### 5. A new metric
Export `platform_scalingschedule_desired_replicas{namespace,name}` as a gauge. Make sure the
series is deleted when the schedule is deleted. *Question:* the schedule controller has no
finalizer. Where can you delete the series? (Hint: the NotFound branch.)

### 6. Detect schedule conflicts in the controller
Webhooks are best-effort. Make the ScalingSchedule controller list the other schedules for the
same target (the `scheduleTargetIndex` exists already). If it isn't the oldest, set
`Ready=False, Reason=Conflict` and don't scale. Test it by calling `Reconcile` directly
(no webhook runs in the controller suite).

### 7. HPA support
Add `spec.autoscaling {minReplicas, maxReplicas, targetCPUUtilizationPercentage}`. When set,
create an `autoscaling/v2` HorizontalPodAutoscaler whose `scaleTargetRef` points at the
**WebApp** (it works thanks to the `/scale` subresource and `status.selector`).
*Design question:* who owns `spec.replicas` now? What should the webhook say if both
`autoscaling` and a ScalingSchedule target the same WebApp?

### 8. Server-side apply
Rewrite `ensure()` for the Service using server-side apply:
`r.Apply(ctx, applyConfig, client.FieldOwner("platform-operator"), client.ForceOwnership)` with
`k8s.io/client-go/applyconfigurations/core/v1`. Compare: is the "don't fight defaults" problem
still there? Look at `metadata.managedFields` on the result.

### 9. Watch only some namespaces
Add a `--watch-namespaces=a,b` flag. Configure `cache.Options{DefaultNamespaces: ...}` in
`ctrl.NewManager`. What happens to the catalog, which lives in a different namespace? (Hint:
`ByObject` or a separate uncached reader.)

### 10. A second API version
Add `v1beta1` of WebApp where `config` is renamed to `configData`. One version is the **hub**,
the version every other one (each a **spoke**) converts to and from. It is usually the newest,
so pick `v1beta1`. Scaffold with
`kubebuilder create api --group platform --version v1beta1 --kind WebApp --resource --controller=false`
and `kubebuilder create webhook --group platform --version v1beta1 --kind WebApp --conversion --spoke v1alpha1`.
Read the [kubebuilder multi-version tutorial](https://book.kubebuilder.io/multiversion-tutorial/tutorial)
first. *Question:* which version should be the **storage** version (`+kubebuilder:storageversion`),
and why can't you just flip it?

### 11. An e2e test
Extend `test/e2e/e2e_test.go`: deploy the operator, apply `config/samples`, and wait until
`kubectl wait --for=condition=Available webapp/storefront` succeeds. This one needs Docker + kind:
`make test-e2e`. It also finally tests your RBAC.

---

When you're done, try writing an operator for something *you* run. A good first target is
anything you currently set up with a README full of `kubectl` commands.
