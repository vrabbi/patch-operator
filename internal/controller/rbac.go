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

// RBAC the operator needs. Kept in one file so the grant is reviewable as a whole rather than
// scattered across reconcilers.
//
// Two notes on how broad this is, since it deserves to be read rather than skimmed:
//
// The wildcard target grant is what makes this an "apply arbitrary fields to arbitrary objects"
// primitive, and it is the reason the authorization model matters so much. Two things bound it: a
// ResourcePatch cannot reach outside its own namespace whatever the requester's rights (enforced
// structurally, in internal/scope), and spec.serviceAccountRef moves the write path onto a tenant
// identity the API server checks on every write. A namespace-only install can drop the
// cluster-scoped half entirely.
//
// The impersonate grant is what serviceAccountRef needs. It is gated at admission by an
// "impersonate" SubjectAccessReview against the requesting principal, so naming a ServiceAccount
// is a way to drop privilege rather than borrow it.

// Contributor and tracker CRDs.
//+kubebuilder:rbac:groups=terasky.com,resources=resourcepatches;clusterresourcepatches;sharedresources;clustersharedresources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=terasky.com,resources=resourcepatches/status;clusterresourcepatches/status;sharedresources/status;clustersharedresources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=terasky.com,resources=resourcepatches/finalizers;clusterresourcepatches/finalizers;sharedresources/finalizers;clustersharedresources/finalizers,verbs=update

// Target objects. Necessarily broad: the operator cannot know at install time which kinds a
// contributor will name.
//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;create;update;patch;delete

// Namespaces, for resolving a ClusterResourcePatch's namespaceSelector.
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// SubjectAccessReview, for the admission checks and the periodic re-check.
//+kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// Impersonation, for spec.serviceAccountRef.
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=impersonate
//+kubebuilder:rbac:groups="",resources=users;groups,verbs=impersonate

// Events, so "who is writing to this object" is answerable from kubectl describe on the target.
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Leader election. Two writers on one target is exactly what this operator exists to prevent, so
// leader election is required rather than optional in HA.
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
