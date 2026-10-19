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
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

// Admission webhooks are HTTPS endpoints the API server calls *before* it
// stores an object:
//
//	kubectl apply -> API server -> mutating webhooks (Default)
//	                            -> schema + CEL validation
//	                            -> validating webhooks (Validate*)
//	                            -> etcd
//
// Prefer CRD markers and CEL rules where they're enough. They run inside
// the API server and can't go down. Use a webhook for logic CEL can't
// express, warnings, or lookups of other objects. See docs/06-webhooks.md.

// webapplog is for logging in this package.
var webapplog = logf.Log.WithName("webapp-resource")

// defaultRequests are applied when a WebApp sets no requests or limits.
// Pods without requests are the first to be evicted under node pressure and
// make capacity planning impossible, so a platform should never allow them.
var defaultRequests = corev1.ResourceList{
	corev1.ResourceCPU:    resource.MustParse("50m"),
	corev1.ResourceMemory: resource.MustParse("64Mi"),
}

// SetupWebAppWebhookWithManager registers the webhook for WebApp in the manager.
func SetupWebAppWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &platformv1alpha1.WebApp{}).
		WithValidator(&WebAppValidator{}).
		WithDefaulter(&WebAppDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-platform-example-com-v1alpha1-webapp,mutating=true,failurePolicy=fail,sideEffects=None,groups=platform.example.com,resources=webapps,verbs=create;update,versions=v1alpha1,name=mwebapp-v1alpha1.kb.io,admissionReviewVersions=v1

// WebAppDefaulter sets default values on WebApps when they are created or
// updated.
type WebAppDefaulter struct{}

// Default implements admission.Defaulter so a webhook will be registered for the Kind WebApp.
//
// Simple scalar defaults (replicas=1, port=8080) live in the CRD as
// +kubebuilder:default markers. This webhook handles a rule markers can't
// express: "fill in requests only if the user set neither requests nor
// limits". If the user set limits, Kubernetes already defaults requests to
// match the limits, and adding our own could make requests > limits.
func (d *WebAppDefaulter) Default(_ context.Context, app *platformv1alpha1.WebApp) error {
	res := &app.Spec.Resources
	if len(res.Requests) == 0 && len(res.Limits) == 0 {
		webapplog.V(1).Info("Defaulting resource requests", "name", app.GetName())
		res.Requests = defaultRequests.DeepCopy()
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-platform-example-com-v1alpha1-webapp,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.example.com,resources=webapps,verbs=create;update,versions=v1alpha1,name=vwebapp-v1alpha1.kb.io,admissionReviewVersions=v1

// WebAppValidator validates WebApps when they are created or updated.
type WebAppValidator struct{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type WebApp.
func (v *WebAppValidator) ValidateCreate(_ context.Context, app *platformv1alpha1.WebApp) (admission.Warnings, error) {
	return validateWebApp(app)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type WebApp.
func (v *WebAppValidator) ValidateUpdate(_ context.Context, _, app *platformv1alpha1.WebApp) (admission.Warnings, error) {
	return validateWebApp(app)
}

// ValidateDelete implements admission.Validator. The marker above doesn't
// include the delete verb, so the API server never calls it.
func (v *WebAppValidator) ValidateDelete(_ context.Context, _ *platformv1alpha1.WebApp) (admission.Warnings, error) {
	return nil, nil
}

// validateWebApp returns warnings (shown to the user, request still allowed)
// and an error (request rejected).
func validateWebApp(app *platformv1alpha1.WebApp) (admission.Warnings, error) {
	var warnings admission.Warnings
	var errs field.ErrorList

	if !imageIsPinned(app.Spec.Image) {
		warnings = append(warnings, fmt.Sprintf(
			"spec.image %q uses an implicit or :latest tag; rollouts will not be reproducible. Pin a version, e.g. nginx:1.27",
			app.Spec.Image))
	}

	// Kubernetes rejects requests > limits on the *Pod*. By then the WebApp
	// is already accepted and the failure is buried in ReplicaSet events.
	// Catching it here gives the user an error straight from kubectl apply.
	resPath := field.NewPath("spec", "resources")
	limits := app.Spec.Resources.Limits
	for _, name := range slices.Sorted(maps.Keys(limits)) {
		req, ok := app.Spec.Resources.Requests[name]
		if !ok {
			continue
		}
		if limit := limits[name]; req.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(resPath.Child("requests").Key(string(name)), req.String(),
				fmt.Sprintf("must be less than or equal to the %s limit (%s)", name, limit.String())))
		}
	}

	if len(errs) > 0 {
		return warnings, apierrors.NewInvalid(
			platformv1alpha1.GroupVersion.WithKind("WebApp").GroupKind(), app.Name, errs)
	}
	return warnings, nil
}

// imageIsPinned reports whether an image reference names a specific version.
//
// References look like [registry[:port]/]repository[:tag][@digest]. The
// registry may contain a colon too (localhost:5000/app), so we look for the
// tag colon only after the last slash.
func imageIsPinned(image string) bool {
	if strings.Contains(image, "@") {
		return true // pinned by digest: the strongest form of pinning
	}
	name := image[strings.LastIndex(image, "/")+1:]
	_, tag, found := strings.Cut(name, ":")
	return found && tag != "" && tag != "latest"
}
