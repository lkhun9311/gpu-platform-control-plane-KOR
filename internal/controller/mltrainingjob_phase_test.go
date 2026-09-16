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

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

// jobWithCondition builds a Job carrying a single status condition.
//
// This is the minimal shape computeMLTJPhase needs to read a Job's Failed or Complete condition.
func jobWithCondition(condType batchv1.JobConditionType, status corev1.ConditionStatus) *batchv1.Job {
	return &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: condType, Status: status}},
		},
	}
}

// workloadAdmitted builds a Workload whose Admitted condition is set to the given state.
func workloadAdmitted(admitted bool) *kueuev1beta1.Workload {
	status := metav1.ConditionFalse
	if admitted {
		status = metav1.ConditionTrue
	}
	return &kueuev1beta1.Workload{
		Status: kueuev1beta1.WorkloadStatus{
			Conditions: []metav1.Condition{{
				Type:   kueuev1beta1.WorkloadAdmitted,
				Status: status,
				Reason: "test",
			}},
		},
	}
}

func TestComputeMLTJPhase(t *testing.T) {
	tests := []struct {
		name      string
		job       *batchv1.Job
		wl        *kueuev1beta1.Workload
		wantPhase string
	}{
		{
			name:      "suspended job with no workload yet is pending",
			job:       &batchv1.Job{Spec: batchv1.JobSpec{Suspend: new(true)}},
			wl:        nil,
			wantPhase: mltjPhasePending,
		},
		{
			name:      "unsuspended job with a workload not yet admitted is pending",
			job:       &batchv1.Job{Spec: batchv1.JobSpec{Suspend: new(false)}},
			wl:        workloadAdmitted(false),
			wantPhase: mltjPhasePending,
		},
		{
			name:      "unsuspended job admitted by kueue with no active pods is admitted",
			job:       &batchv1.Job{Spec: batchv1.JobSpec{Suspend: new(false)}},
			wl:        workloadAdmitted(true),
			wantPhase: mltjPhaseAdmitted,
		},
		{
			name:      "job with active pods is running even though the workload is admitted",
			job:       &batchv1.Job{Status: batchv1.JobStatus{Active: 1}},
			wl:        workloadAdmitted(true),
			wantPhase: mltjPhaseRunning,
		},
		{
			name:      "job with the complete condition true is succeeded",
			job:       jobWithCondition(batchv1.JobComplete, corev1.ConditionTrue),
			wl:        workloadAdmitted(true),
			wantPhase: mltjPhaseSucceeded,
		},
		{
			name:      "job with the failed condition true is failed",
			job:       jobWithCondition(batchv1.JobFailed, corev1.ConditionTrue),
			wl:        workloadAdmitted(true),
			wantPhase: mltjPhaseFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPhase, gotCond := computeMLTJPhase(tt.job, tt.wl)
			if gotPhase != tt.wantPhase {
				t.Errorf("computeMLTJPhase() phase = %q, want %q", gotPhase, tt.wantPhase)
			}
			if gotCond.Type != mltjCondAdmitted {
				t.Errorf("computeMLTJPhase() condition type = %q, want %q", gotCond.Type, mltjCondAdmitted)
			}
		})
	}
}
