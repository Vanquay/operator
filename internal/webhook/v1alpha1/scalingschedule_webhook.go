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

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
	"github.com/example/platform-operator/internal/schedule"
)

// SetupScalingScheduleWebhookWithManager registers the webhook for ScalingSchedule in the manager.
func SetupScalingScheduleWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &platformv1alpha1.ScalingSchedule{}).
		// The validator reads other objects. It gets the *uncached* API
		// reader: an admission decision should rest on what is stored right
		// now, not on a cache that may be a few milliseconds behind.
		// Schedules are written rarely, so the extra API calls are cheap.
		// Dependencies are passed in as struct fields, not globals, which
		// keeps the validator easy to test.
		WithValidator(&ScalingScheduleValidator{Client: mgr.GetAPIReader()}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-platform-example-com-v1alpha1-scalingschedule,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.example.com,resources=scalingschedules,verbs=create;update,versions=v1alpha1,name=vscalingschedule-v1alpha1.kb.io,admissionReviewVersions=v1

// ScalingScheduleValidator validates ScalingSchedules when they are created
// or updated.
type ScalingScheduleValidator struct {
	Client client.Reader
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type ScalingSchedule.
func (v *ScalingScheduleValidator) ValidateCreate(ctx context.Context, ss *platformv1alpha1.ScalingSchedule) (admission.Warnings, error) {
	return v.validate(ctx, ss)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type ScalingSchedule.
func (v *ScalingScheduleValidator) ValidateUpdate(ctx context.Context, _, ss *platformv1alpha1.ScalingSchedule) (admission.Warnings, error) {
	return v.validate(ctx, ss)
}

// ValidateDelete implements admission.Validator. Not called: the marker
// above doesn't include the delete verb.
func (v *ScalingScheduleValidator) ValidateDelete(_ context.Context, _ *platformv1alpha1.ScalingSchedule) (admission.Warnings, error) {
	return nil, nil
}

func (v *ScalingScheduleValidator) validate(ctx context.Context, ss *platformv1alpha1.ScalingSchedule) (admission.Warnings, error) {
	gk := platformv1alpha1.GroupVersion.WithKind("ScalingSchedule").GroupKind()

	// 1. Checks that need Go, such as "is this a real time zone?". This is
	// the same function the controller uses, so they always agree.
	sched, errs := schedule.FromSpec(ss.Spec)
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(gk, ss.Name, errs)
	}

	// 2. Overlapping windows are legal (first one wins), but often a mistake.
	// Warnings let the request through and print a message in kubectl.
	var warnings admission.Warnings
	for _, o := range sched.Overlaps() {
		warnings = append(warnings, fmt.Sprintf(
			"windows %q and %q overlap; %q takes precedence while both are active", o.First, o.Second, o.First))
	}

	// 3. Cross-object rules need the API. Two schedules on one WebApp would
	// fight over its replica count forever, so we allow only one.
	//
	// Caveat: two schedules created at the same instant could both pass this
	// check. Webhooks are best-effort for invariants across objects.
	// Reconcilers must still behave sensibly if one slips through.
	var others platformv1alpha1.ScalingScheduleList
	if err := v.Client.List(ctx, &others, client.InNamespace(ss.Namespace)); err != nil {
		return warnings, apierrors.NewInternalError(fmt.Errorf("listing ScalingSchedules: %w", err))
	}
	for _, other := range others.Items {
		if other.Name != ss.Name && other.Spec.TargetRef.Name == ss.Spec.TargetRef.Name {
			return warnings, apierrors.NewInvalid(gk, ss.Name, field.ErrorList{
				field.Forbidden(field.NewPath("spec", "targetRef", "name"),
					fmt.Sprintf("WebApp %q is already targeted by ScalingSchedule %q", ss.Spec.TargetRef.Name, other.Name)),
			})
		}
	}

	// 4. A missing target is only a warning. With `kubectl apply -f dir/`,
	// the schedule may arrive a moment before its WebApp, and the controller
	// waits for it.
	app := &platformv1alpha1.WebApp{}
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: ss.Namespace, Name: ss.Spec.TargetRef.Name}, app)
	switch {
	case apierrors.IsNotFound(err):
		warnings = append(warnings, fmt.Sprintf(
			"WebApp %q does not exist yet; the schedule will apply once it is created", ss.Spec.TargetRef.Name))
	case err != nil:
		return warnings, apierrors.NewInternalError(fmt.Errorf("reading target WebApp: %w", err))
	}

	return warnings, nil
}
