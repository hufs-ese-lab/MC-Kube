/*
Copyright 2025.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MCKubeSpec defines the desired state of MCKube
type MCKubeSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Node    string `json:"node,omitempty"`
	PodName string `json:"podname,omitempty"`
	// Criticality defines the priority level: LOW, MIDDLE, or HIGH
	// +kubebuilder:validation:Enum=LOW;MIDDLE;HIGH
	Criticality              string `json:"criticality,omitempty"`
	PressuredDeadlinesTotal  int    `json:"pressuredDeadlinesTotal,omitempty"`
	PressuredDeadlinesPeriod int    `json:"pressuredDeadlinesPeriod,omitempty"`

	// RT Cgroup settings
	RTSettings *RTSettings `json:"rtSettings,omitempty"`
}

// RTSettings defines RT cgroup configuration
type RTSettings struct {
	Period     int     `json:"period"`         // RT period in microseconds
	RuntimeLow int     `json:"budget_lo"`      // Initial conservative RT runtime in microseconds
	RuntimeHi  int     `json:"budget_hi"`      // Elevated RT runtime after overrun detection in microseconds
	Core       *string `json:"core,omitempty"` // CPU core range (e.g., "2-3")
}

// MCKubeStatus defines the observed state of MCKube
type MCKubeStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// CurrentRuntime tracks whether the pod is using runtime_low or runtime_hi
	CurrentRuntime string `json:"currentRuntime,omitempty"` // "low" or "hi"
	// LastOverrunTime tracks when the last overrun was detected
	LastOverrunTime *metav1.Time `json:"lastOverrunTime,omitempty"`
	// AllocatedCore tracks the actual core allocated to the pod (may differ from Spec.RTSettings.Core due to preemption)
	AllocatedCore string `json:"allocatedCore,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MCKube is the Schema for the mckubes API
type MCKube struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCKubeSpec   `json:"spec,omitempty"`
	Status MCKubeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MCKubeList contains a list of MCKube
type MCKubeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCKube `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCKube{}, &MCKubeList{})
}
