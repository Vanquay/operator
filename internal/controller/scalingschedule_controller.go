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

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
	"github.com/example/platform-operator/internal/metrics"
	"github.com/example/platform-operator/internal/schedule"
)

const (
	// scheduleTargetIndex maps a WebApp name to the schedules targeting it.
	scheduleTargetIndex = ".spec.targetRef.name"

	// maxRequeue is a safety net: even if we compute the next transition
	// wrongly, we look again at least this often.
	maxRequeue = time.Hour
	// transitionSlack wakes us just after a boundary rather than exactly on
	// it, so the new window is definitely active when we re-evaluate.
	transitionSlack = time.Second
)

// ScalingScheduleReconciler reconciles a ScalingSchedule object.
type ScalingScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// Clock is the source of "now". Production passes clock.RealClock{};
	// tests pass a fake clock so they can jump to 09:00 on a Monday.
	// Never call time.Now() directly in logic you want to test.
	Clock clock.PassiveClock
}

// +kubebuilder:rbac:groups=platform.example.com,resources=scalingschedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.example.com,resources=scalingschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.example.com,resources=webapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.example.com,resources=webapps/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile works out which window is active and scales the target WebApp.
//
// Unlike the WebApp controller, this one is driven mainly by time. Nothing in
// the cluster changes at 08:00 on Monday, so no watch would wake us. Instead
// every pass returns RequeueAfter = "time until the next window boundary".
func (r *ScalingScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ss := &platformv1alpha1.ScalingSchedule{}
	if err := r.Get(ctx, req.NamespacedName, ss); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	original := ss.DeepCopy()

	if ss.Spec.Suspend {
		ss.Status.NextTransitionTime = nil
		r.setReady(ss, metav1.ConditionFalse, platformv1alpha1.ReasonSuspended, "spec.suspend is true")
		return ctrl.Result{}, r.patchStatus(ctx, ss, original)
	}

	// The webhook rejects invalid schedules, but webhooks can be disabled or
	// bypassed, so the controller re-checks. Retrying cannot fix a bad spec,
	// so we return no error. A user edit bumps the generation and requeues us.
	sched, errs := schedule.FromSpec(ss.Spec)
	if len(errs) > 0 {
		msg := errs.ToAggregate().Error()
		r.setReady(ss, metav1.ConditionFalse, platformv1alpha1.ReasonInvalidSchedule, msg)
		r.Recorder.Eventf(ss, nil, corev1.EventTypeWarning, platformv1alpha1.ReasonInvalidSchedule, "Evaluate", msg)
		return ctrl.Result{}, r.patchStatus(ctx, ss, original)
	}

	now := r.Clock.Now()
	result := sched.Evaluate(now)
	windowName := ""
	if result.Window != nil {
		windowName = result.Window.Name
	}
	ss.Status.ActiveWindow = windowName
	ss.Status.DesiredReplicas = new(result.Replicas)
	ss.Status.NextTransitionTime = nil
	if !result.NextTransition.IsZero() {
		ss.Status.NextTransitionTime = &metav1.Time{Time: result.NextTransition}
	}
	requeue := ctrl.Result{RequeueAfter: requeueAfter(now, result.NextTransition)}

	scaled, err := r.scaleTarget(ctx, ss, result.Replicas)
	switch {
	case apierrors.IsNotFound(err):
		r.setReady(ss, metav1.ConditionFalse, platformv1alpha1.ReasonTargetNotFound,
			fmt.Sprintf("WebApp %q not found", ss.Spec.TargetRef.Name))
		// The WebApp watch below wakes us when the target is created.
		return requeue, r.patchStatus(ctx, ss, original)
	case err != nil:
		r.setReady(ss, metav1.ConditionFalse, platformv1alpha1.ReasonScaleFailed, err.Error())
		if statusErr := r.patchStatus(ctx, ss, original); statusErr != nil {
			log.Error(statusErr, "Failed to update status after scale error")
		}
		return ctrl.Result{}, fmt.Errorf("scaling WebApp %q: %w", ss.Spec.TargetRef.Name, err)
	}

	if scaled {
		ss.Status.LastScaleTime = &metav1.Time{Time: now}
		label := windowName
		if label == "" {
			label = metrics.NoWindowLabel
		}
		metrics.ScheduleScaleOperations.WithLabelValues(ss.Namespace, ss.Name, label).Inc()
		r.Recorder.Eventf(ss, nil, corev1.EventTypeNormal, "Scaled", "Scale",
			"scaled WebApp %q to %d replicas (window: %s)", ss.Spec.TargetRef.Name, result.Replicas, label)
		log.Info("Scaled target", "webapp", ss.Spec.TargetRef.Name, "replicas", result.Replicas, "window", label)
	}

	r.setReady(ss, metav1.ConditionTrue, platformv1alpha1.ReasonScheduleApplied,
		fmt.Sprintf("WebApp %q has %d desired replicas", ss.Spec.TargetRef.Name, result.Replicas))
	return requeue, r.patchStatus(ctx, ss, original)
}

// scaleTarget sets the WebApp's replica count through its /scale
// subresource and reports whether anything changed.
//
// Why /scale instead of editing spec.replicas directly? /scale is a small,
// generic contract (autoscaling/v1 Scale) that every scalable resource
// shares: Deployments, StatefulSets and our WebApp, thanks to the
// +kubebuilder:subresource:scale marker. The HorizontalPodAutoscaler and
// `kubectl scale` use the same contract. It also needs a narrower
// permission (webapps/scale) than updating whole WebApps.
func (r *ScalingScheduleReconciler) scaleTarget(ctx context.Context, ss *platformv1alpha1.ScalingSchedule, replicas int32) (bool, error) {
	app := &platformv1alpha1.WebApp{ObjectMeta: metav1.ObjectMeta{Namespace: ss.Namespace, Name: ss.Spec.TargetRef.Name}}

	scaled := false
	// scale carries the resourceVersion we read. If the WebApp controller
	// writes the WebApp in between (it patches status often), the update
	// fails with 409 Conflict. A conflict means "re-read and try again", so
	// we retry the read-modify-write here instead of failing the reconcile.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		scale := &autoscalingv1.Scale{}
		if err := r.SubResource("scale").Get(ctx, app, scale); err != nil {
			return err
		}
		if scale.Spec.Replicas == replicas {
			return nil
		}
		scale.Spec.Replicas = replicas
		if err := r.SubResource("scale").Update(ctx, app, client.WithSubResourceBody(scale)); err != nil {
			return err
		}
		scaled = true
		return nil
	})
	return scaled, err
}

func (r *ScalingScheduleReconciler) setReady(ss *platformv1alpha1.ScalingSchedule, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type:               platformv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ss.Generation,
	})
}

func (r *ScalingScheduleReconciler) patchStatus(ctx context.Context, ss, original *platformv1alpha1.ScalingSchedule) error {
	ss.Status.ObservedGeneration = ss.Generation
	if equality.Semantic.DeepEqual(original.Status, ss.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, ss, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching status: %w", err)
	}
	return nil
}

// requeueAfter turns "the next boundary is at T" into a delay.
func requeueAfter(now, next time.Time) time.Duration {
	if next.IsZero() {
		return maxRequeue
	}
	return min(next.Sub(now)+transitionSlack, maxRequeue)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScalingScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &platformv1alpha1.ScalingSchedule{}, scheduleTargetIndex,
		func(obj client.Object) []string {
			return []string{obj.(*platformv1alpha1.ScalingSchedule).Spec.TargetRef.Name}
		}); err != nil {
		return fmt.Errorf("indexing %s: %w", scheduleTargetIndex, err)
	}

	// Predicates filter events before they reach the workqueue.
	// GenerationChangedPredicate drops updates where metadata.generation is
	// unchanged, which means status-only or metadata-only updates:
	//   - our own status patches don't immediately retrigger us, and
	//   - a WebApp's status changing as its pods start doesn't wake every
	//     schedule. Only spec changes, such as someone running
	//     `kubectl scale`, get through.
	onlySpecChanges := builder.WithPredicates(predicate.GenerationChangedPredicate{})

	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ScalingSchedule{}, onlySpecChanges).
		Watches(&platformv1alpha1.WebApp{}, handler.EnqueueRequestsFromMapFunc(r.schedulesForWebApp), onlySpecChanges).
		Named("scalingschedule").
		Complete(r)
}

// schedulesForWebApp maps a WebApp event to the schedules that target it.
// This is what makes the schedule re-assert its replica count if someone
// scales the WebApp by hand, the same way an HPA would.
func (r *ScalingScheduleReconciler) schedulesForWebApp(ctx context.Context, app client.Object) []reconcile.Request {
	var schedules platformv1alpha1.ScalingScheduleList
	if err := r.List(ctx, &schedules,
		client.InNamespace(app.GetNamespace()),
		client.MatchingFields{scheduleTargetIndex: app.GetName()},
	); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list ScalingSchedules for WebApp", "webapp", client.ObjectKeyFromObject(app))
		return nil
	}

	requests := make([]reconcile.Request, 0, len(schedules.Items))
	for _, ss := range schedules.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ss)})
	}
	return requests
}
