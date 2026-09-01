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

import "fmt"

// JobUIDLabelKey is Kueue's label on a Workload carrying the owning Job's UID (kueue.x-k8s.io/job-uid). The
// collector reads it into ObservedObject.JobUIDLabel, and the join cross-checks it against the CONTROLLER
// ownerReference so a Workload cannot be attributed to a Job it merely happens to sit beside.
const JobUIDLabelKey = "kueue.x-k8s.io/job-uid"

// OwnerRef is the identity-bearing part of a Kubernetes ownerReference the join needs: whose UID, of what
// kind, and whether it is the controlling owner. Only the single controller reference establishes the
// parent; a non-controller reference (a soft link) must never re-attribute an object.
type OwnerRef struct {
	UID        string
	Kind       string
	Controller bool
}

// ObservedObject is the minimal identity and ownership a collector extracts from a watched object, so the
// join can be unit-tested without a live API server.
type ObservedObject struct {
	// Kind is "MLTrainingJob" | "Job" | "Workload" | "Pod".
	Kind string
	// Name is the object's own name; for an MLTrainingJob it IS the trace job name.
	Name string
	// UID is the object's UID, the immutable attempt/generation identity the ledger keys on.
	UID string
	// Owners are the object's ownerReferences; only the controller reference of the expected parent kind is
	// followed.
	Owners []OwnerRef
	// JobUIDLabel is a Workload's kueue.x-k8s.io/job-uid value (empty for other kinds); it must equal the
	// owning Job's UID or the chain is inconsistent.
	JobUIDLabel string
}

// Object kinds the join understands.
const (
	kindMLTrainingJob = "MLTrainingJob"
	kindJob           = "Job"
	kindWorkload      = "Workload"
	kindPod           = "Pod"
)

// ResolveTraceJobs attributes every observed Job, Workload, and Pod to the trace job (MLTrainingJob) it
// descends from, following ONLY the controller ownerReference at each hop up to the root MLTrainingJob
// (whose name is the trace row name).
//
// It returns objectUID -> trace job name for every object. It errors on any object that does not resolve
// through a single controller owner of the expected kind to exactly one MLTrainingJob, an orphan whose
// owning Job was never seen, a missing UID, or a Workload whose kueue.x-k8s.io/job-uid label disagrees with
// its controller Job's UID, because a name-only or non-controller join would silently merge two generations
// of a recreated object into one timeline.
func ResolveTraceJobs(objs []ObservedObject) (map[string]string, error) {
	seen := map[string]bool{}
	for _, o := range objs {
		if o.UID == "" {
			return nil, fmt.Errorf("%s %q has no UID", o.Kind, o.Name)
		}
		if seen[o.UID] {
			return nil, fmt.Errorf("duplicate object UID %q", o.UID)
		}
		seen[o.UID] = true
	}

	name := map[string]string{} // objectUID -> trace job name
	jobUID := map[string]bool{} // UIDs known to be Jobs

	// Roots: an MLTrainingJob's own name is the trace job name.
	for _, o := range objs {
		if o.Kind == kindMLTrainingJob {
			name[o.UID] = o.Name
		}
	}
	// Jobs are controlled by an MLTrainingJob.
	for _, o := range objs {
		if o.Kind != kindJob {
			continue
		}
		ownerUID, err := controllerOwner(o, kindMLTrainingJob)
		if err != nil {
			return nil, err
		}
		parent, ok := name[ownerUID]
		if !ok {
			return nil, fmt.Errorf("job %q is controlled by an unobserved MLTrainingJob %q", o.Name, ownerUID)
		}
		name[o.UID] = parent
		jobUID[o.UID] = true
	}
	// Workloads and Pods are controlled by a Job.
	for _, o := range objs {
		switch o.Kind {
		case kindMLTrainingJob, kindJob:
			continue
		case kindWorkload, kindPod:
		default:
			return nil, fmt.Errorf("unknown object kind %q for %q", o.Kind, o.Name)
		}
		ownerUID, err := controllerOwner(o, kindJob)
		if err != nil {
			return nil, err
		}
		if !jobUID[ownerUID] {
			return nil, fmt.Errorf("%s %q is controlled by an unobserved Job %q", o.Kind, o.Name, ownerUID)
		}
		if o.Kind == kindWorkload && o.JobUIDLabel != ownerUID {
			return nil, fmt.Errorf("workload %q job-uid label %q disagrees with controller Job UID %q", o.Name, o.JobUIDLabel, ownerUID)
		}
		name[o.UID] = name[ownerUID]
	}
	return name, nil
}

// controllerOwner returns the UID of the object's single controller ownerReference of the expected kind,
// erroring on none or more than one, so only the controlling parent (not an incidental soft owner) is
// followed.
func controllerOwner(o ObservedObject, expectedKind string) (string, error) {
	var uid string
	matches := 0
	for _, ref := range o.Owners {
		if ref.Controller && ref.Kind == expectedKind {
			uid = ref.UID
			matches++
		}
	}
	if matches != 1 {
		return "", fmt.Errorf("%s %q has %d controller %s owners, want exactly 1", o.Kind, o.Name, matches, expectedKind)
	}
	return uid, nil
}
