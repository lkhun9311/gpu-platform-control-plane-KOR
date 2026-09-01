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

package queuelab

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObservedFrom builds the join's ObservedObject from any Kubernetes object's metadata, so the collector can
// feed real Pods, Jobs, Workloads, and MLTrainingJobs into ResolveTraceJobs without the join needing to
// import every type.
//
// It records only the CONTROLLER flag on each owner reference (not merely that a reference exists), so the
// UID-chain join follows the controlling parent alone; and for a Workload it lifts Kueue's job-uid label so
// the join can cross-check it against the controller Job.
func ObservedFrom(kind string, obj metav1.Object) ObservedObject {
	o := ObservedObject{Kind: kind, Name: obj.GetName(), UID: string(obj.GetUID())}
	for _, r := range obj.GetOwnerReferences() {
		o.Owners = append(o.Owners, OwnerRef{
			UID:        string(r.UID),
			Kind:       r.Kind,
			Controller: r.Controller != nil && *r.Controller,
		})
	}
	if kind == kindWorkload {
		o.JobUIDLabel = obj.GetLabels()[JobUIDLabelKey]
	}
	return o
}
