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

// Plain Go unit tests for the pure builder functions. They need no API
// server, so they run in milliseconds:
//
//	go test ./internal/controller/ -run 'TestComputeConfigHash|TestMutate' -v
//
// (-run picks top-level Test functions by regex. The Ginkgo suite lives in
// TestControllers, so it is skipped and envtest never starts.)

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

func TestComputeConfigHash(t *testing.T) {
	base := computeConfigHash(map[string]string{"A": "1", "B": "2"}, "rv-1")

	tests := []struct {
		name     string
		config   map[string]string
		secretRV string
		wantSame bool
	}{
		{"same content is stable", map[string]string{"B": "2", "A": "1"}, "rv-1", true},
		{"value change", map[string]string{"A": "1", "B": "3"}, "rv-1", false},
		{"key added", map[string]string{"A": "1", "B": "2", "C": "3"}, "rv-1", false},
		{"secret changed", map[string]string{"A": "1", "B": "2"}, "rv-2", false},
		// %q quoting stops "A=1\nB" from colliding with key "A" plus value "1\nB".
		{"no ambiguity between keys and values", map[string]string{"A": "1\n\"B\"=\"2\""}, "rv-1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeConfigHash(tc.config, tc.secretRV)
			if (got == base) != tc.wantSame {
				t.Errorf("computeConfigHash() = %s, base = %s, wantSame = %v", got, base, tc.wantSame)
			}
		})
	}
}

// TestMutateDeploymentPreservesServerDefaults checks that updating an
// existing Deployment keeps fields we don't own, which the API server
// filled in.
func TestMutateDeploymentPreservesServerDefaults(t *testing.T) {
	app := &platformv1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "prod"},
		Spec: platformv1alpha1.WebAppSpec{
			Image:    "shop:2.0",
			Replicas: new(int32(3)),
			Port:     9000,
		},
	}

	// Simulate a Deployment as read back from the API server.
	originalSelector := &metav1.LabelSelector{MatchLabels: map[string]string{"legacy": "selector"}}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "prod", ResourceVersion: "42",
			Labels: map[string]string{"team": "payments"}},
		Spec: appsv1.DeploymentSpec{
			Selector: originalSelector,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyAlways,
					Containers: []corev1.Container{{
						Name:                   appContainerName,
						Image:                  "shop:1.0",
						ImagePullPolicy:        corev1.PullIfNotPresent,
						TerminationMessagePath: "/dev/termination-log",
					}},
				},
			},
		},
	}

	mutateDeployment(dep, app, "abc123")

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "shop:2.0" {
		t.Errorf("image = %q, want shop:2.0", c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent || c.TerminationMessagePath != "/dev/termination-log" {
		t.Errorf("server-defaulted container fields were overwritten: %+v", c)
	}
	if dep.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("restartPolicy was overwritten")
	}
	if dep.Spec.Selector != originalSelector {
		t.Errorf("selector of an existing Deployment must never change (it is immutable)")
	}
	if dep.Labels["team"] != "payments" {
		t.Errorf("labels set by others were removed: %v", dep.Labels)
	}
	if got := *dep.Spec.Replicas; got != 3 {
		t.Errorf("replicas = %d, want 3", got)
	}
	if got := dep.Spec.Template.Annotations[configHashAnnotation]; got != "abc123" {
		t.Errorf("config hash annotation = %q, want abc123", got)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected the existing app container to be reused, got %d containers", len(dep.Spec.Template.Spec.Containers))
	}
}

func TestMutateDeploymentSetsSelectorOnCreate(t *testing.T) {
	app := &platformv1alpha1.WebApp{ObjectMeta: metav1.ObjectMeta{Name: "shop"}, Spec: platformv1alpha1.WebAppSpec{Image: "shop:1", Port: 8080}}
	dep := &appsv1.Deployment{} // no resourceVersion: not created yet

	mutateDeployment(dep, app, "h")

	if dep.Spec.Selector == nil || dep.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != "shop" {
		t.Fatalf("selector = %v, want instance=shop", dep.Spec.Selector)
	}
	for k, v := range dep.Spec.Selector.MatchLabels {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("pod template labels must match the selector; missing %s=%s", k, v)
		}
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("nil spec.replicas should mean 1, got %d", *dep.Spec.Replicas)
	}
}
