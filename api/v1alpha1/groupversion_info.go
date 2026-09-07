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

// Package v1alpha1 contains API Schema definitions for the terasky.com v1alpha1 API group.
//
// Four kinds in two scope-matched pairs, mirroring Role/ClusterRole: ResourcePatch +
// SharedResource are namespaced and confined to one namespace; ClusterResourcePatch +
// ClusterSharedResource are cluster-scoped and reach across namespaces.
//
// +kubebuilder:object:generate=true
// +groupName=terasky.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version these objects are registered under.
	GroupVersion = schema.GroupVersion{Group: "terasky.com", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a scheme.
	//
	// Built on runtime.SchemeBuilder rather than controller-runtime's scheme.Builder helper, which
	// is deprecated precisely because an api package should depend on little more than
	// apimachinery.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers every kind in this group-version.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ResourcePatch{}, &ResourcePatchList{},
		&ClusterResourcePatch{}, &ClusterResourcePatchList{},
		&SharedResource{}, &SharedResourceList{},
		&ClusterSharedResource{}, &ClusterSharedResourceList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
