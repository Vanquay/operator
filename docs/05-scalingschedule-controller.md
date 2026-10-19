# 5. The ScalingSchedule controller

Files: `internal/controller/scalingschedule_controller.go`, `internal/schedule/`.

This controller is interesting because its main input is **the clock**. Nothing in the cluster
changes at 08:00 on Monday, so no watch event will wake it.

## The loop

```go
sched, errs := schedule.FromSpec(ss.Spec)  // parse + validate (shared with the webhook)
result := sched.Evaluate(r.Clock.Now())    // which window? how many replicas? next change?
scaled, err := r.scaleTarget(ctx, ss, result.Replicas)
...
return ctrl.Result{RequeueAfter: requeueAfter(now, result.NextTransition)}, r.patchStatus(...)
```

### `RequeueAfter`: wake me later

Instead of polling every few seconds, each reconcile computes **when the answer will next
change** and asks to be woken just after it (`+1s` slack), capped at `maxRequeue` (1h) as a
safety net. The schedule from `config/samples`, evaluated on a Monday at 17:30:

```
active: business-hours (4 replicas)   next transition: 18:00   → RequeueAfter = 30m1s
```

The requeue lives only in the operator's memory. If the operator restarts, every object is
reconciled again at startup anyway (the informer lists everything), so nothing is lost. Level
triggering pays off again.

### Keep the hard logic pure

All time-window maths lives in `internal/schedule`, which has **no Kubernetes calls**. That buys:

- tests in plain Go (`go test ./internal/schedule/`, 95%+ coverage, milliseconds),
- reuse: the webhook calls the same `FromSpec`, so "valid" means the same thing in both places,
- readability: the reconciler is ~100 lines of orchestration.

Two details are worth reading in `schedule.go`:

- **Overnight windows** (`22:00` → `06:00`): `ActiveAt` checks "started today and past start" *or*
  "started yesterday and before end".
- **Daylight saving**: boundaries are built with `time.Date(y, m, d+offset, hh, mm, 0, 0, loc)`,
  never `now.Add(24*time.Hour)`. `TestEvaluateAcrossDaylightSaving` shows the 19-hour day in
  London when the clocks go forward.

### Injecting the clock

```go
type ScalingScheduleReconciler struct {
    ...
    Clock clock.PassiveClock   // clock.RealClock{} in main.go, a fake in tests
}
```

Any code that calls `time.Now()` directly can only be tested in real time. With an injected
clock the test jumps from Monday 09:00 to Monday 18:00:01 instantly:

```go
fakeClock.SetTime(time.Date(2026, time.October, 19, 18, 0, 1, 0, london))
reconcileSchedule("shop-hours")
Expect(webAppReplicas("shop")).To(Equal(int32(2)))
```

## Talking to another controller's resource: `/scale`

The schedule never edits the WebApp object directly. It uses the **scale subresource**:

```go
scale := &autoscalingv1.Scale{}
r.SubResource("scale").Get(ctx, app, scale)
scale.Spec.Replicas = replicas
r.SubResource("scale").Update(ctx, app, client.WithSubResourceBody(scale))
```

- It's the same contract the HPA and `kubectl scale` use, so this controller would work
  unchanged against a Deployment.
- It needs only `webapps/scale` permission, not the right to rewrite whole WebApps.
- The `Scale` carries the `resourceVersion` it was read at. If the WebApp changes in between
  (the WebApp controller patches its status often), the update gets **409 Conflict**.
  `retry.RetryOnConflict` re-reads and retries. That is optimistic concurrency, the Kubernetes
  replacement for locks.

The chain of events when a window opens:

```
ScalingSchedule reconciler ──PUT /scale──► WebApp.spec.replicas = 4  (generation++)
WebApp reconciler (For watch) ──────────► Deployment.spec.replicas = 4
Deployment controller ──────────────────► ReplicaSet ──► 2 new Pods
WebApp reconciler (Owns watch) ─────────► WebApp.status.readyReplicas = 4
```

Two independent controllers cooperate only through the API, with no direct calls between them.

## Predicates: filtering events

```go
onlySpecChanges := builder.WithPredicates(predicate.GenerationChangedPredicate{})

For(&platformv1alpha1.ScalingSchedule{}, onlySpecChanges).
Watches(&platformv1alpha1.WebApp{}, handler.EnqueueRequestsFromMapFunc(r.schedulesForWebApp), onlySpecChanges).
```

`metadata.generation` changes only when **spec** changes. Filtering on it means:

- our own status patches don't immediately retrigger the schedule (the WebApp controller instead
  relies on skipping no-op writes; both approaches are valid),
- WebApp **status** churn (pods starting) doesn't wake every schedule,
- a human running `kubectl scale webapp storefront --replicas=1` *does* change spec, so the
  schedule reconciles and **re-asserts** its replica count, exactly like an HPA would.

Other built-in predicates: `LabelChangedPredicate`, `AnnotationChangedPredicate`,
`ResourceVersionChangedPredicate`, plus `predicate.Funcs{...}` for custom logic.

## Conflicts between writers

Two schedules targeting one WebApp would flip its replicas back and forth forever. The webhook
rejects a second schedule for the same target, and the `targetRef` CEL rule makes the target
immutable. A webhook check across objects is still **best-effort** (two creates at the same instant
could both pass), so production code should also detect and report the conflict in the
controller. That's one of the [exercises](10-exercises.md).

Next: [6. Admission webhooks](06-webhooks.md)
