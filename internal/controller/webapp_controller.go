/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
	"github.com/example/platform-operator/internal/catalog"
	"github.com/example/platform-operator/internal/metrics"
)

const (
	// webAppFinalizer blocks deletion of a WebApp until the operator has
	// removed its catalog entry. Finalizer names are namespaced like labels.
	webAppFinalizer = "platform.example.com/catalog-cleanup"

	// secretRefIndex is the name of a cache index mapping a Secret name to the
	// WebApps that reference it. The name is arbitrary but must be unique.
	secretRefIndex = ".spec.secretRef.name"

	// conflictRequeueDelay is how long to wait before retrying after a 409
	// Conflict. Long enough for the cache to catch up with the newer write.
	conflictRequeueDelay = 500 * time.Millisecond
)

// WebAppReconciler reconciles a WebApp object.
type WebAppReconciler struct {
	// Embedding client.Client promotes its methods onto the reconciler, so we
	// can call r.Get(...) instead of r.Client.Get(...).
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Catalog  catalog.Catalog
}

// The RBAC markers below are turned into config/rbac/role.yaml by
// `make manifests`. If the operator needs a new permission, add a marker
// here. Forgetting one shows up as "forbidden" errors at runtime.
//
// +kubebuilder:rbac:groups=platform.example.com,resources=webapps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=platform.example.com,resources=webapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.example.com,resources=webapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves the cluster towards the state described by one WebApp.
//
// Key ideas (docs/01-operators-101.md explains them in depth):
//   - Level-triggered: we never ask "what changed?". We read the whole
//     desired state (the WebApp) and make the world match it. A missed or
//     duplicated event therefore does no harm.
//   - Idempotent: running Reconcile twice in a row must be harmless.
//   - Return an error to retry with exponential backoff. Return
//     RequeueAfter to check again later. Return an empty Result when done
//     and let watches wake us up.
func (r *WebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	err := r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		// 409 Conflict means our cached copy was stale: someone else (often
		// the ScalingSchedule controller, via /scale) wrote the object after
		// we read it. That is routine under optimistic concurrency, not a
		// failure, so don't log it as an error. Just try again shortly with
		// fresh data.
		logf.FromContext(ctx).V(1).Info("Conflict writing object; retrying with a fresh copy", "error", err.Error())
		return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
	}
	// This controller never needs a timed requeue: watches on the WebApp,
	// its children and its Secret cover every input. Errors are retried with
	// exponential backoff by controller-runtime.
	return ctrl.Result{}, err
}

func (r *WebAppReconciler) reconcile(ctx context.Context, req ctrl.Request) error {
	log := logf.FromContext(ctx)

	// 1. Fetch the WebApp. Reads come from the manager's in-memory cache,
	// which informers keep in sync, so they are cheap.
	app := &platformv1alpha1.WebApp{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		// NotFound means it was deleted after the event was queued. Its
		// children carry owner references, so Kubernetes' garbage collector
		// removes them. Nothing to do.
		return client.IgnoreNotFound(err)
	}

	// 2. Being deleted? Run our cleanup, then let the deletion finish.
	if !app.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, app)
	}

	// 3. Make sure our finalizer is present before we create anything
	// outside the WebApp's namespace (the catalog entry).
	if controllerutil.AddFinalizer(app, webAppFinalizer) {
		if err := r.Update(ctx, app); err != nil {
			return fmt.Errorf("adding finalizer: %w", err)
		}
	}

	// Snapshot the object. At the end we compare against it to skip no-op
	// status writes, and to build a patch containing only what changed.
	original := app.DeepCopy()

	if app.Spec.Paused {
		setCondition(app, platformv1alpha1.ConditionProgressing, metav1.ConditionFalse,
			platformv1alpha1.ReasonPaused, "spec.paused is true; child resources are not being updated")
		return r.patchStatus(ctx, app, original)
	}

	// 4. Resolve inputs from other objects.
	secretVersion, err := r.secretVersion(ctx, app)
	if apierrors.IsNotFound(err) {
		msg := fmt.Sprintf("Secret %q not found; waiting for it to be created", app.Spec.SecretRef.Name)
		setCondition(app, platformv1alpha1.ConditionDegraded, metav1.ConditionTrue, platformv1alpha1.ReasonSecretNotFound, msg)
		r.Recorder.Eventf(app, nil, corev1.EventTypeWarning, platformv1alpha1.ReasonSecretNotFound, "Reconcile", msg)
		// No error on purpose: retrying won't make the Secret appear. The
		// Secret watch in SetupWithManager wakes us up when it is created.
		return r.patchStatus(ctx, app, original)
	}
	if err != nil {
		return fmt.Errorf("reading Secret: %w", err)
	}
	configHash := computeConfigHash(app.Spec.Config, secretVersion)

	// 5. Create or update every child object.
	dep, err := r.reconcileChildren(ctx, app, configHash)
	if apierrors.IsConflict(err) {
		return err // routine, not Degraded: Reconcile requeues quietly
	}
	if err != nil {
		setCondition(app, platformv1alpha1.ConditionDegraded, metav1.ConditionTrue, platformv1alpha1.ReasonReconcileFailed, err.Error())
		r.Recorder.Eventf(app, nil, corev1.EventTypeWarning, platformv1alpha1.ReasonReconcileFailed, "Reconcile", err.Error())
		if statusErr := r.patchStatus(ctx, app, original); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return err
	}

	// 6. Report what we observed.
	computeStatus(app, dep, configHash)
	metrics.WebAppReadyReplicas.WithLabelValues(app.Namespace, app.Name).Set(float64(app.Status.ReadyReplicas))

	if original.Status.ConfigHash != "" && original.Status.ConfigHash != configHash {
		r.Recorder.Eventf(app, dep, corev1.EventTypeNormal, "RolloutTriggered", "Reconcile",
			"configuration changed (hash %s -> %s); restarting pods", original.Status.ConfigHash, configHash)
	}
	return r.patchStatus(ctx, app, original)
}

// reconcileChildren brings every owned object in line with the spec and
// returns the Deployment so the caller can derive status from it.
func (r *WebAppReconciler) reconcileChildren(ctx context.Context, app *platformv1alpha1.WebApp, configHash string) (*appsv1.Deployment, error) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName(app), Namespace: app.Namespace}}
	if err := r.ensure(ctx, app, cm, func() { mutateConfigMap(cm, app) }); err != nil {
		return nil, err
	}

	dep := &appsv1.Deployment{ObjectMeta: childMeta(app)}
	if err := r.ensure(ctx, app, dep, func() { mutateDeployment(dep, app, configHash) }); err != nil {
		return nil, err
	}

	svc := &corev1.Service{ObjectMeta: childMeta(app)}
	if err := r.ensure(ctx, app, svc, func() { mutateService(svc, app) }); err != nil {
		return nil, err
	}

	if err := r.reconcileIngress(ctx, app); err != nil {
		return nil, err
	}

	entry := catalog.Entry{Namespace: app.Namespace, Name: app.Name, Image: app.Spec.Image, URL: appURL(app)}
	if err := r.Catalog.Register(ctx, entry); err != nil {
		return nil, fmt.Errorf("registering in catalog: %w", err)
	}
	return dep, nil
}

// reconcileIngress handles a child that only exists when the spec asks for it.
func (r *WebAppReconciler) reconcileIngress(ctx context.Context, app *platformv1alpha1.WebApp) error {
	ing := &networkingv1.Ingress{ObjectMeta: childMeta(app)}

	if app.Spec.Ingress != nil {
		return r.ensure(ctx, app, ing, func() { mutateIngress(ing, app) })
	}

	// spec.ingress is unset. If we created an Ingress earlier, delete it.
	if err := r.Get(ctx, client.ObjectKeyFromObject(ing), ing); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(ing, app) {
		return nil // Someone else's Ingress with the same name; leave it alone.
	}
	if err := r.Delete(ctx, ing); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting Ingress: %w", err)
	}
	r.Recorder.Eventf(app, ing, corev1.EventTypeNormal, "IngressDeleted", "Reconcile", "spec.ingress removed; deleted Ingress %q", ing.Name)
	return nil
}

// ensure creates obj, or updates it to match what mutate writes, and makes
// the WebApp its controlling owner.
//
// controllerutil.CreateOrUpdate does the Get -> mutate -> compare -> Create
// or Update dance for us. It only sends an Update when the mutate function
// actually changed something, which is why the mutate functions must leave
// server-defaulted fields untouched.
func (r *WebAppReconciler) ensure(ctx context.Context, app *platformv1alpha1.WebApp, obj client.Object, mutate func()) error {
	kind := r.kindOf(obj)
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		// Never adopt an object we didn't create. Without this check, a
		// WebApp named "payments" would take over an existing hand-made
		// Deployment called "payments".
		if obj.GetResourceVersion() != "" && !metav1.IsControlledBy(obj, app) {
			return fmt.Errorf("object exists and is not managed by WebApp %q", app.Name)
		}
		mutate()
		// The owner reference does two jobs: Owns() watches route the
		// child's events back to this WebApp, and the garbage collector
		// deletes the child when the WebApp is deleted.
		return controllerutil.SetControllerReference(app, obj, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconciling %s %q: %w", kind, obj.GetName(), err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("Reconciled child object", "kind", kind, "name", obj.GetName(), "operation", op)
	}
	return nil
}

// finalize runs cleanup that garbage collection cannot do for us, then
// removes the finalizer so the API server can delete the object.
func (r *WebAppReconciler) finalize(ctx context.Context, app *platformv1alpha1.WebApp) error {
	if !controllerutil.ContainsFinalizer(app, webAppFinalizer) {
		return nil
	}
	// Returning an error keeps the finalizer, and Reconcile is retried. The
	// WebApp stays "Terminating" until cleanup succeeds. That is exactly
	// what we want: no orphaned catalog entries.
	if err := r.Catalog.Deregister(ctx, app.Namespace, app.Name); err != nil {
		return fmt.Errorf("removing catalog entry: %w", err)
	}
	metrics.WebAppReadyReplicas.DeleteLabelValues(app.Namespace, app.Name)

	controllerutil.RemoveFinalizer(app, webAppFinalizer)
	if err := r.Update(ctx, app); err != nil {
		return fmt.Errorf("removing finalizer: %w", err)
	}
	logf.FromContext(ctx).Info("Finalized WebApp")
	return nil
}

// secretVersion returns the resourceVersion of the referenced Secret, or ""
// if there is no secretRef.
//
// We only need to know *that* the Secret changed, not what is in it. So the
// operator watches Secrets as metadata only (see SetupWithManager) and never
// holds secret values in memory. Note the special PartialObjectMetadata
// type: asking the cache for a full corev1.Secret would start a second,
// full Secret informer.
func (r *WebAppReconciler) secretVersion(ctx context.Context, app *platformv1alpha1.WebApp) (string, error) {
	if app.Spec.SecretRef == nil {
		return "", nil
	}
	secret := &metav1.PartialObjectMetadata{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	key := client.ObjectKey{Namespace: app.Namespace, Name: app.Spec.SecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return "", err
	}
	return secret.ResourceVersion, nil
}

// patchStatus writes status through the /status subresource, but only if it
// changed. Skipping no-op writes matters: every write is a watch event that
// would trigger another reconcile.
func (r *WebAppReconciler) patchStatus(ctx context.Context, app, original *platformv1alpha1.WebApp) error {
	app.Status.ObservedGeneration = app.Generation
	if equality.Semantic.DeepEqual(original.Status, app.Status) {
		return nil
	}
	// A merge patch sends just the changed fields, and needs no matching
	// resourceVersion, so it doesn't conflict with users editing spec.
	if err := r.Status().Patch(ctx, app, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching status: %w", err)
	}
	return nil
}

func (r *WebAppReconciler) kindOf(obj client.Object) string {
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return fmt.Sprintf("%T", obj)
	}
	return gvk.Kind
}

func childMeta(app *platformv1alpha1.WebApp) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}
}

// SetupWithManager tells the manager which events should trigger Reconcile.
func (r *WebAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A field index lets us ask the cache "which WebApps use Secret X?"
	// without listing and filtering every WebApp in the namespace.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &platformv1alpha1.WebApp{}, secretRefIndex,
		func(obj client.Object) []string {
			app := obj.(*platformv1alpha1.WebApp)
			if app.Spec.SecretRef == nil || app.Spec.SecretRef.Name == "" {
				return nil
			}
			return []string{app.Spec.SecretRef.Name}
		}); err != nil {
		return fmt.Errorf("indexing %s: %w", secretRefIndex, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		// The primary resource: every WebApp create/update/delete.
		For(&platformv1alpha1.WebApp{}).
		// Children: an event on an object whose controller owner reference
		// points at a WebApp enqueues that WebApp. This is how a pod
		// becoming ready (which updates Deployment status) reaches us.
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		// A resource we don't own: map each Secret event to the WebApps
		// that reference it. Metadata-only keeps Secret data out of memory.
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.webAppsForSecret)).
		// Different WebApps may reconcile in parallel. The same WebApp is
		// never reconciled by two workers at once; the workqueue guarantees it.
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Named("webapp").
		Complete(r)
}

// webAppsForSecret maps a Secret event to reconcile requests.
func (r *WebAppReconciler) webAppsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	var apps platformv1alpha1.WebAppList
	if err := r.List(ctx, &apps,
		client.InNamespace(secret.GetNamespace()),
		client.MatchingFields{secretRefIndex: secret.GetName()},
	); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list WebApps for Secret", "secret", client.ObjectKeyFromObject(secret))
		return nil
	}

	requests := make([]reconcile.Request, 0, len(apps.Items))
	for _, app := range apps.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&app)})
	}
	return requests
}
