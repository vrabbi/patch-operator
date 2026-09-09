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
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rp
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.apply.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ResourcePatch contributes a slice of configuration to a shared object in its own namespace.
//
// A ResourcePatch can only ever touch objects in its own namespace. That is structural, not a
// policy check: the target namespace is forced to the ResourcePatch's namespace at admission and
// re-checked at reconcile, and no field expresses anything else. The property therefore holds
// regardless of how privileged the principal creating it is — which matters because a patch
// emitted by Crossplane or kro is admitted as the orchestrator's near-cluster-admin ServiceAccount.
type ResourcePatch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourcePatchSpec   `json:"spec,omitempty"`
	Status ResourcePatchStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourcePatchList contains a list of ResourcePatch.
type ResourcePatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourcePatch `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=crp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.target.namespace`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterResourcePatch contributes a slice of configuration to a shared object in any namespace,
// or to a cluster-scoped object.
//
// Identical spec to ResourcePatch — this is one API with two reaches, not two APIs. It is the
// privileged kind: it is the only one that can cross a namespace boundary, so it should be a
// platform-team grant rather than something handed to tenant Compositions.
type ClusterResourcePatch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourcePatchSpec   `json:"spec,omitempty"`
	Status ResourcePatchStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterResourcePatchList contains a list of ClusterResourcePatch.
type ClusterResourcePatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterResourcePatch `json:"items"`
}
