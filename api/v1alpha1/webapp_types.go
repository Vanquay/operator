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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// This file defines the WebApp API: the Go structs below ARE the CRD schema.
// `make manifests` runs controller-gen, which reads these structs plus the
// `+kubebuilder:...` marker comments and writes config/crd/bases/*.yaml.
//
// Markers are plain Go comments, so the compiler ignores them. Only
// controller-gen understands them. See docs/03-api-design.md.

// Condition types reported in WebApp.status.conditions.
// Following the Kubernetes API conventions, each condition is a separate
// "is this true?" question rather than a single "phase" string.
const (
	// ConditionAvailable is True when every desired replica is ready to serve.
	ConditionAvailable = "Available"
	// ConditionProgressing is True while a rollout is in flight.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is True when the operator cannot converge the app,
	// for example because a referenced Secret is missing.
	ConditionDegraded = "Degraded"
)

// Reasons are short CamelCase machine-readable explanations for a condition.
const (
	ReasonReplicasAvailable = "ReplicasAvailable"
	ReasonReplicasNotReady  = "ReplicasNotReady"
	ReasonScaledToZero      = "ScaledToZero"
	ReasonRollingOut        = "RollingOut"
	ReasonRolloutComplete   = "RolloutComplete"
	ReasonPaused            = "Paused"
	ReasonAsExpected        = "AsExpected"
	ReasonSecretNotFound    = "SecretNotFound"
	ReasonReconcileFailed   = "ReconcileFailed"
)

// WebAppSpec defines the desired state of WebApp.
//
// The CEL rule below runs inside the API server (no webhook required) and
// rejects objects that try to set PORT themselves, because the operator
// injects PORT from spec.port.
// +kubebuilder:validation:XValidation:rule="!has(self.env) || !self.env.exists(e, e.name == 'PORT')",message="env var PORT is reserved; set spec.port instead"
type WebAppSpec struct {
	// image is the container image to run, e.g. "nginx:1.27".
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// replicas is the desired number of pods. It is a pointer so we can tell
	// "not set" (nil, defaulted to 1) apart from an explicit 0 (scale to zero).
	// This field is also exposed through the /scale subresource, which is how
	// `kubectl scale` and ScalingSchedule change it.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// port is the TCP port the container listens on. The operator exposes it
	// through a Service on port 80 and passes it to the container as $PORT.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// env is a list of extra environment variables for the container.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// config holds non-secret key/value settings. The operator renders them
	// into a ConfigMap and exposes every key as an environment variable.
	// Changing a value triggers a rolling restart of the pods.
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[-._a-zA-Z0-9]+$'))",message="config keys may only contain letters, digits, '-', '_' and '.'"
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// secretRef names an existing Secret (same namespace) whose keys are
	// exposed as environment variables. The operator watches that Secret and
	// rolls the pods whenever it changes.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// resources are the container's compute requests and limits. If you leave
	// requests empty, the defaulting webhook fills in small defaults.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// ingress, when set, exposes the app outside the cluster.
	// Removing this block deletes the Ingress.
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`

	// paused stops the operator from changing child resources. It is useful
	// while debugging a broken rollout by hand.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// IngressSpec configures external HTTP access to a WebApp.
type IngressSpec struct {
	// host is the DNS name to route, e.g. "shop.example.com".
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	// +required
	Host string `json:"host"`

	// path is the URL path prefix to route.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:default="/"
	// +optional
	Path string `json:"path,omitempty"`

	// className selects the IngressClass (e.g. "nginx"). Leave empty to use
	// the cluster default.
	// +optional
	ClassName *string `json:"className,omitempty"`
}

// WebAppStatus defines the observed state of WebApp.
// Only the operator writes status; users write spec.
type WebAppStatus struct {
	// observedGeneration is the metadata.generation the operator last acted on.
	// If it is lower than metadata.generation, the status is stale.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// replicas is the number of pods the Deployment currently has.
	// Read by the /scale subresource.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// readyReplicas is the number of pods passing their readiness checks.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// selector is the label selector of the pods, as a string. The /scale
	// subresource needs it so tools like the HorizontalPodAutoscaler can
	// find the pods.
	// +optional
	Selector string `json:"selector,omitempty"`

	// url is where the app can be reached: the Ingress URL if one exists,
	// otherwise the in-cluster Service DNS name.
	// +optional
	URL string `json:"url,omitempty"`

	// configHash is a hash of the config and Secret version currently rolled
	// out. When it changes, the pods restart.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// conditions represent the current state of the WebApp resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=wa,categories=platform
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WebApp is the Schema for the webapps API.
//
// A WebApp is a "golden path" for running a stateless HTTP service. From one
// small object the operator creates and owns a ConfigMap, Deployment, Service
// and (optionally) an Ingress, and registers the app in the cluster catalog.
type WebApp struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WebApp
	// +required
	Spec WebAppSpec `json:"spec"`

	// status defines the observed state of WebApp
	// +optional
	Status WebAppStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WebAppList contains a list of WebApp
type WebAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WebApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WebApp{}, &WebAppList{})
		return nil
	})
}
