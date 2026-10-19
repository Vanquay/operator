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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types and reasons reported in ScalingSchedule.status.conditions.
const (
	// ConditionReady is True when the schedule is valid, its target exists and
	// the target has been scaled to the desired replica count.
	ConditionReady = "Ready"

	ReasonScheduleApplied = "ScheduleApplied"
	ReasonTargetNotFound  = "TargetNotFound"
	ReasonInvalidSchedule = "InvalidSchedule"
	ReasonSuspended       = "Suspended"
	ReasonScaleFailed     = "ScaleFailed"
)

// Weekday is a three-letter English day name.
// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type Weekday string

// ScalingScheduleSpec defines the desired state of ScalingSchedule.
type ScalingScheduleSpec struct {
	// targetRef names the WebApp (in the same namespace) to scale.
	// The CEL rule makes it immutable: re-pointing a schedule would leave the
	// old target stuck at whatever size it had, so we force delete + create.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetRef is immutable"
	// +required
	TargetRef TargetReference `json:"targetRef"`

	// timeZone is an IANA time zone name such as "Europe/London" or
	// "America/New_York". Window times are evaluated in this zone, including
	// daylight-saving changes. CEL cannot check this, so the validating
	// webhook does.
	// +kubebuilder:default="UTC"
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// defaultReplicas is the replica count used when no window is active.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +required
	DefaultReplicas int32 `json:"defaultReplicas"`

	// windows are the time ranges with their replica counts. If several
	// windows are active at the same moment, the first one in the list wins.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +required
	Windows []ScheduleWindow `json:"windows"`

	// suspend pauses the schedule. The target keeps its current size.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// TargetReference identifies the WebApp to scale.
type TargetReference struct {
	// name of the WebApp.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// ScheduleWindow is a recurring daily time range.
//
// If end is earlier than start, the window crosses midnight. For example
// start=22:00 end=06:00 with days=[Fri] runs from Friday 22:00 to Saturday
// 06:00.
// +kubebuilder:validation:XValidation:rule="self.start != self.end",message="start and end must differ"
type ScheduleWindow struct {
	// name identifies the window and appears in status.activeWindow.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// days the window starts on. Empty means every day.
	// +listType=set
	// +kubebuilder:validation:MaxItems=7
	// +optional
	Days []Weekday `json:"days,omitempty"`

	// start time, 24h "HH:MM", inclusive.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +required
	Start string `json:"start"`

	// end time, 24h "HH:MM", exclusive.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +required
	End string `json:"end"`

	// replicas while this window is active.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +required
	Replicas int32 `json:"replicas"`
}

// ScalingScheduleStatus defines the observed state of ScalingSchedule.
type ScalingScheduleStatus struct {
	// observedGeneration is the metadata.generation the operator last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// activeWindow is the name of the window in effect, or empty when the
	// default applies.
	// +optional
	ActiveWindow string `json:"activeWindow,omitempty"`

	// desiredReplicas is what the schedule wants right now. It is a pointer so
	// that 0 still shows up in the output.
	// +optional
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty"`

	// lastScaleTime is when the operator last changed the target's replicas.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// nextTransitionTime is when the active window will next change.
	// +optional
	NextTransitionTime *metav1.Time `json:"nextTransitionTime,omitempty"`

	// conditions represent the current state of the ScalingSchedule resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ss,categories=platform
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.status.activeWindow`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Next",type=string,JSONPath=`.status.nextTransitionTime`,description="When the active window next changes (UTC). type=string because kubectl renders date columns as an age, which is <invalid> for future times."
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ScalingSchedule is the Schema for the scalingschedules API.
//
// A ScalingSchedule changes a WebApp's replica count by time of day, e.g.
// "10 replicas on weekdays 08:00-18:00, otherwise 2". It acts like a
// time-based HorizontalPodAutoscaler and talks to the WebApp only through its
// /scale subresource.
type ScalingSchedule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ScalingSchedule
	// +required
	Spec ScalingScheduleSpec `json:"spec"`

	// status defines the observed state of ScalingSchedule
	// +optional
	Status ScalingScheduleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ScalingScheduleList contains a list of ScalingSchedule
type ScalingScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ScalingSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ScalingSchedule{}, &ScalingScheduleList{})
		return nil
	})
}
