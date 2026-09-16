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

import "testing"

func ctrl(uid, kind string) OwnerRef { return OwnerRef{UID: uid, Kind: kind, Controller: true} }

// fullChain builds one complete MLTrainingJob -> Job -> {Workload, Pod} controller-ownership chain for "a1".
func fullChain() []ObservedObject {
	return []ObservedObject{
		{Kind: kindMLTrainingJob, Name: "a1", UID: "mltj-1"},
		{Kind: kindJob, Name: "a1", UID: "job-1", Owners: []OwnerRef{ctrl("mltj-1", kindMLTrainingJob)}},
		{Kind: kindWorkload, Name: "wl-a1", UID: "wl-1", Owners: []OwnerRef{ctrl("job-1", kindJob)}, JobUIDLabel: "job-1"},
		{Kind: kindPod, Name: "a1-pod", UID: "pod-1", Owners: []OwnerRef{ctrl("job-1", kindJob)}},
	}
}

func TestResolveTraceJobsFullChain(t *testing.T) {
	got, err := ResolveTraceJobs(fullChain())
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"mltj-1", "job-1", "wl-1", "pod-1"} {
		if got[uid] != "a1" {
			t.Fatalf("object %s resolved to %q, want a1", uid, got[uid])
		}
	}
}

func TestResolveTraceJobsIgnoresNonControllerOwner(t *testing.T) {
	// A Job carries a non-controller ownerReference to a2's MLTrainingJob but is actually controlled by a1's.
	// Following the controller reference only, it must resolve to a1, not be pulled toward a2 or rejected as
	// ambiguous.
	objs := []ObservedObject{
		{Kind: kindMLTrainingJob, Name: "a1", UID: "mltj-1"},
		{Kind: kindMLTrainingJob, Name: "a2", UID: "mltj-2"},
		{Kind: kindJob, Name: "a1", UID: "job-1", Owners: []OwnerRef{
			ctrl("mltj-1", kindMLTrainingJob),
			{UID: "mltj-2", Kind: kindMLTrainingJob, Controller: false},
		}},
		{Kind: kindPod, Name: "a1-pod", UID: "pod-1", Owners: []OwnerRef{ctrl("job-1", kindJob)}},
	}
	got, err := ResolveTraceJobs(objs)
	if err != nil {
		t.Fatal(err)
	}
	if got["job-1"] != "a1" || got["pod-1"] != "a1" {
		t.Fatalf("non-controller owner leaked attribution: job-1=%q pod-1=%q", got["job-1"], got["pod-1"])
	}
}

func TestResolveTraceJobsRejectsJobUIDLabelMismatch(t *testing.T) {
	// The Workload's kueue job-uid label points at a different Job than its controller reference: a broken
	// chain a name-only join would have accepted.
	objs := fullChain()
	for i := range objs {
		if objs[i].Kind == kindWorkload {
			objs[i].JobUIDLabel = "job-999"
		}
	}
	if _, err := ResolveTraceJobs(objs); err == nil {
		t.Fatalf("a job-uid label disagreeing with the controller must error")
	}
}

func TestResolveTraceJobsRejectsOrphan(t *testing.T) {
	// A Pod whose controller Job was never observed cannot be attributed, so it errors rather than being
	// dropped (which would silently shrink an arm's evidence).
	objs := []ObservedObject{
		{Kind: kindMLTrainingJob, Name: "a1", UID: "mltj-1"},
		{Kind: kindJob, Name: "a1", UID: "job-1", Owners: []OwnerRef{ctrl("mltj-1", kindMLTrainingJob)}},
		{Kind: kindPod, Name: "orphan", UID: "pod-x", Owners: []OwnerRef{ctrl("job-unknown", kindJob)}},
	}
	if _, err := ResolveTraceJobs(objs); err == nil {
		t.Fatalf("an orphan pod must error")
	}
}

func TestResolveTraceJobsRejectsMissingUID(t *testing.T) {
	objs := []ObservedObject{{Kind: kindMLTrainingJob, Name: "a1", UID: ""}}
	if _, err := ResolveTraceJobs(objs); err == nil {
		t.Fatalf("an object without a UID must error")
	}
}

func TestResolveTraceJobsTwoIndependentChains(t *testing.T) {
	// Two trace jobs must not cross-attribute: each object resolves to its own root.
	objs := append(fullChain(),
		ObservedObject{Kind: kindMLTrainingJob, Name: "b1", UID: "mltj-2"},
		ObservedObject{Kind: kindJob, Name: "b1", UID: "job-2", Owners: []OwnerRef{ctrl("mltj-2", kindMLTrainingJob)}},
		ObservedObject{Kind: kindPod, Name: "b1-pod", UID: "pod-2", Owners: []OwnerRef{ctrl("job-2", kindJob)}},
	)
	got, err := ResolveTraceJobs(objs)
	if err != nil {
		t.Fatal(err)
	}
	if got["pod-1"] != "a1" || got["pod-2"] != "b1" {
		t.Fatalf("pods cross-attributed: pod-1=%q pod-2=%q", got["pod-1"], got["pod-2"])
	}
}
