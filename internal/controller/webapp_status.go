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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

// setCondition is a small wrapper that always stamps observedGeneration.
// meta.SetStatusCondition only changes lastTransitionTime when the status
// value flips, so calling this on every reconcile is safe.
func setCondition(app *platformv1alpha1.WebApp, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
	})
}

// computeStatus derives the WebApp's status from its Deployment.
//
// The operator doesn't watch pods itself. The Deployment controller (part of
// kube-controller-manager) already sums them up in deployment.status, and
// we read that summary. Controllers building on other controllers' status is
// how Kubernetes is layered.
func computeStatus(app *platformv1alpha1.WebApp, dep *appsv1.Deployment, configHash string) {
	st := &app.Status
	st.Replicas = dep.Status.Replicas
	st.ReadyReplicas = dep.Status.ReadyReplicas
	st.Selector = labels.SelectorFromSet(selectorLabels(app)).String()
	st.URL = appURL(app)
	st.ConfigHash = configHash

	desired := desiredReplicas(app)

	// Available: can the app serve traffic?
	switch {
	case desired == 0:
		setCondition(app, platformv1alpha1.ConditionAvailable, metav1.ConditionFalse,
			platformv1alpha1.ReasonScaledToZero, "spec.replicas is 0")
	case deploymentConditionTrue(dep, appsv1.DeploymentAvailable) && dep.Status.ReadyReplicas > 0:
		setCondition(app, platformv1alpha1.ConditionAvailable, metav1.ConditionTrue,
			platformv1alpha1.ReasonReplicasAvailable,
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, desired))
	default:
		setCondition(app, platformv1alpha1.ConditionAvailable, metav1.ConditionFalse,
			platformv1alpha1.ReasonReplicasNotReady,
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, desired))
	}

	// Progressing: is a rollout still underway?
	if rolloutComplete(dep, desired) {
		setCondition(app, platformv1alpha1.ConditionProgressing, metav1.ConditionFalse,
			platformv1alpha1.ReasonRolloutComplete, "all replicas run the latest spec")
	} else {
		setCondition(app, platformv1alpha1.ConditionProgressing, metav1.ConditionTrue,
			platformv1alpha1.ReasonRollingOut,
			fmt.Sprintf("%d/%d replicas updated", dep.Status.UpdatedReplicas, desired))
	}

	// Degraded: we got this far, so the operator itself is healthy.
	setCondition(app, platformv1alpha1.ConditionDegraded, metav1.ConditionFalse,
		platformv1alpha1.ReasonAsExpected, "")
}

// rolloutComplete mirrors what `kubectl rollout status` checks.
func rolloutComplete(dep *appsv1.Deployment, desired int32) bool {
	return dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas == desired &&
		dep.Status.Replicas == desired &&
		dep.Status.AvailableReplicas == desired
}

func deploymentConditionTrue(dep *appsv1.Deployment, t appsv1.DeploymentConditionType) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == t {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
