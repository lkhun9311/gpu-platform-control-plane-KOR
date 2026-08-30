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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestObservedFromExtractsControllerOwner(t *testing.T) {
	ctrlTrue := true
	ctrlFalse := false
	obj := &metav1.ObjectMeta{
		Name: "wl-a1",
		UID:  "wl-1",
		Labels: map[string]string{
			JobUIDLabelKey: "job-1",
		},
		OwnerReferences: []metav1.OwnerReference{
			{UID: "job-1", Kind: "Job", Controller: &ctrlTrue},
			{UID: "other", Kind: "Job", Controller: &ctrlFalse},
			{UID: "nocontroller", Kind: "Job"}, // Controller nil -> false
		},
	}
	got := ObservedFrom(kindWorkload, obj)
	if got.UID != "wl-1" || got.Name != "wl-a1" {
		t.Fatalf("identity = %s/%s", got.UID, got.Name)
	}
	if got.JobUIDLabel != "job-1" {
		t.Fatalf("job-uid label = %q, want job-1", got.JobUIDLabel)
	}
	if len(got.Owners) != 3 {
		t.Fatalf("want 3 owner refs, got %d", len(got.Owners))
	}
	// Exactly the first ref is the controller.
	var controllers int
	for _, o := range got.Owners {
		if o.Controller {
			controllers++
			if o.UID != "job-1" {
				t.Fatalf("controller owner = %q, want job-1", o.UID)
			}
		}
	}
	if controllers != 1 {
		t.Fatalf("want exactly one controller owner, got %d", controllers)
	}
}

func TestObservedFromFeedsResolveTraceJobs(t *testing.T) {
	// The adapter output must slot straight into ResolveTraceJobs: an adapted chain resolves end to end.
	ctrl := true
	mltj := &metav1.ObjectMeta{Name: "a1", UID: "mltj-1"}
	job := &metav1.ObjectMeta{Name: "a1", UID: "job-1", OwnerReferences: []metav1.OwnerReference{{UID: "mltj-1", Kind: kindMLTrainingJob, Controller: &ctrl}}}
	pod := &metav1.ObjectMeta{Name: "a1-pod", UID: "pod-1", OwnerReferences: []metav1.OwnerReference{{UID: "job-1", Kind: kindJob, Controller: &ctrl}}}

	objs := []ObservedObject{
		ObservedFrom(kindMLTrainingJob, mltj),
		ObservedFrom(kindJob, job),
		ObservedFrom(kindPod, pod),
	}
	got, err := ResolveTraceJobs(objs)
	if err != nil {
		t.Fatal(err)
	}
	if got["pod-1"] != "a1" {
		t.Fatalf("adapted chain did not resolve: pod-1=%q", got["pod-1"])
	}
}
