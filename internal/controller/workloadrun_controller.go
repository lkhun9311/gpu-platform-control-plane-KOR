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
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

const (
	// workloadRunPoll is the DEFAULT cadence for looking at an open window.
	//
	// It is a default rather than a constant because the right cadence depends on what is being watched: a
	// recovery that takes two seconds is invisible to a five-second poll, and a run recorded that way says
	// the platform never left its healthy state. That is not a small inaccuracy -- it is the trail reporting
	// a constant, which is what this whole type exists to stop.
	workloadRunPoll = 5 * time.Second
	// workloadRunMaxGapPolls is how many missed looks make a hole rather than a hiccup.
	//
	// A single missed reconcile is ordinary -- a leader election, a slow apiserver -- and refusing on it
	// would make the type useless. Three consecutive is a controller that was not running, and a verdict
	// computed across that is a claim about a window nobody watched.
	//
	// It is a multiple of the POLL rather than a duration, so tightening the cadence tightens the tolerance
	// with it. A fixed tolerance beside a configurable poll would silently accept three-second holes in a
	// run that looks every second.
	workloadRunMaxGapPolls = 3
	// workloadRunHealthyPhase is the phase both watchable kinds use for "serving normally".
	//
	// InferenceDeployment and NodeHealth happen to agree on the word, and this constant is deliberately
	// separate from both: if either renames its phase, this must be a decision rather than a silent
	// reinterpretation of every recorded run.
	workloadRunHealthyPhase = "Ready"
)

// WorkloadRunReconciler assembles the evidence trail for one injected failure.
//
// It does NOT inject anything. The scenarios are shell scripts (hack/chaos-*.sh) that a human runs, and
// keeping the injection out of the controller keeps the recorder honest: a component that both causes the
// failure and judges the recovery can always be accused of having judged its own work.
type WorkloadRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Now is injectable so tests can drive a window without waiting for one.
	Now func() time.Time
	// Poll is how often an open window is looked at; zero selects workloadRunPoll.
	//
	// Configurable because a recovery faster than the cadence is invisible, and a trail that missed it
	// reports a platform that never changed. hack/m7-evidence-trail.sh tightens it for exactly that reason:
	// a Pod on kind is replaced in a couple of seconds.
	Poll time.Duration
}

// poll is the cadence this reconciler runs at.
func (r *WorkloadRunReconciler) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return workloadRunPoll
}

// maxGap is how long the trail may go unobserved before it stops being one, derived from the cadence.
func (r *WorkloadRunReconciler) maxGap() time.Duration {
	return workloadRunMaxGapPolls * r.poll()
}

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=workloadruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=workloadruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=nodehealths,verbs=get;list;watch

func (r *WorkloadRunReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *WorkloadRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var run platformv1.WorkloadRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal states are terminal. A run that has been refused is not retried into a pass, and one that
	// completed is not re-judged against a target that has moved on since.
	switch run.Status.Phase {
	case platformv1.WorkloadRunComplete, platformv1.WorkloadRunRefused:
		return ctrl.Result{}, nil
	}

	now := r.now()

	// The expectation must not move under the observation. Editing the spec mid-window would leave a trail
	// assembled against one deadline and judged against another, which is the post-hoc move every other
	// gate in this repository exists to prevent.
	if run.Status.Phase == platformv1.WorkloadRunObserving && run.Status.ObservedGeneration != run.Generation {
		return r.refuse(ctx, &run, fmt.Sprintf(
			"the spec changed during the window (generation %d observed, %d now): the trail was assembled "+
				"against one expectation and would be judged against another",
			run.Status.ObservedGeneration, run.Generation))
	}

	state, found, err := r.observeTarget(ctx, &run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if run.Status.Phase == "" || run.Status.Phase == platformv1.WorkloadRunPending {
		if !found {
			// Refused rather than failed. A target that was never there is not a platform that did not
			// recover, and the two must be distinguishable from the record alone.
			return r.refuse(ctx, &run, fmt.Sprintf(
				"target %s %q does not exist, so there is nothing this run could be evidence about",
				run.Spec.Target.Kind, targetKey(run.Spec.Target)))
		}
		run.Status.Phase = platformv1.WorkloadRunObserving
		run.Status.StartedAt = &metav1.Time{Time: now}
		run.Status.ObservedGeneration = run.Generation
		r.record(&run, now, state)
		run.Status.LastObservedAt = &metav1.Time{Time: now}
		if err := r.Status().Update(ctx, &run); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Observing", "run", req.String(), "scenario", run.Spec.Scenario)
		return ctrl.Result{RequeueAfter: r.poll()}, nil
	}

	// The gap check comes before anything is recorded, so a controller that was down cannot append an
	// observation that makes the trail look continuous.
	if last := run.Status.LastObservedAt; last != nil && now.Sub(last.Time) > r.maxGap() {
		return r.refuse(ctx, &run, fmt.Sprintf(
			"nothing was observed for %s, which is longer than the %s this run's poll interval allows: the "+
				"trail has a hole and a verdict across it would be a claim about a window nobody watched",
			now.Sub(last.Time).Round(time.Second), r.maxGap()))
	}

	if !found {
		return r.refuse(ctx, &run, fmt.Sprintf(
			"target %s %q was deleted during the window; a run cannot be evidence about an object that "+
				"stopped existing, and calling that a failure to recover would blame the platform for it",
			run.Spec.Target.Kind, targetKey(run.Spec.Target)))
	}

	elapsed := int32(now.Sub(run.Status.StartedAt.Time).Seconds())
	r.record(&run, now, state)
	run.Status.LastObservedAt = &metav1.Time{Time: now}

	if elapsed < run.Spec.ObservationWindowSeconds {
		if err := r.Status().Update(ctx, &run); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.poll()}, nil
	}

	// Nothing was ever observed to fail, so there is no recovery to judge.
	//
	// Refused rather than Recovered, because a pass here would be the strongest possible claim resting on
	// the weakest possible evidence: the scenario was injected and the platform's own phase never moved. The
	// honest reading is that the run did not see the failure -- the injection missed, or the outage was
	// shorter than the poll -- and both send an operator somewhere a false pass would not.
	if !run.Status.ObservedUnhealthy {
		return r.refuse(ctx, &run, fmt.Sprintf(
			"the target was never observed unhealthy during the %ds window, so nothing was observed to "+
				"recover from; a verdict here would rest on a failure this run did not see",
			run.Spec.ObservationWindowSeconds))
	}

	// The window closed over a covered observation, so there is something to stand behind.
	run.Status.Phase = platformv1.WorkloadRunComplete
	if at := run.Status.RecoveredAtSeconds; at != nil && *at <= run.Spec.RecoversWithinSeconds {
		run.Status.Verdict = platformv1.VerdictRecovered
		run.Status.Reason = fmt.Sprintf("observed %s at %ds, within the declared %ds",
			workloadRunHealthyPhase, *at, run.Spec.RecoversWithinSeconds)
	} else {
		run.Status.Verdict = platformv1.VerdictNotRecovered
		if at == nil {
			run.Status.Reason = fmt.Sprintf("never observed %s during the %ds window",
				workloadRunHealthyPhase, run.Spec.ObservationWindowSeconds)
		} else {
			// A late recovery is a fail rather than a slow pass, because the deadline was declared first.
			run.Status.Reason = fmt.Sprintf("observed %s at %ds, after the declared %ds",
				workloadRunHealthyPhase, *at, run.Spec.RecoversWithinSeconds)
		}
	}
	if err := r.Status().Update(ctx, &run); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Complete", "run", req.String(), "verdict", run.Status.Verdict)
	return ctrl.Result{}, nil
}

// record appends state to the trail if it differs from the last thing seen, and notes a first recovery.
//
// Only changes are appended: a trail carrying every poll would say how often the controller ran rather
// than what the platform did.
func (r *WorkloadRunReconciler) record(run *platformv1.WorkloadRun, now time.Time, state string) {
	healthy := state == workloadRunHealthyPhase
	var elapsed int32
	if run.Status.StartedAt != nil {
		elapsed = int32(now.Sub(run.Status.StartedAt.Time).Seconds())
	}

	n := len(run.Status.Observations)
	if n == 0 || run.Status.Observations[n-1].State != state {
		run.Status.Observations = append(run.Status.Observations, platformv1.WorkloadRunObservation{
			ElapsedSeconds: elapsed,
			State:          state,
			Healthy:        healthy,
		})
	}
	// A recovery is a return to health AFTER something was observed to fail, and the distinction is not
	// pedantic: the target is healthy when a run starts, so crediting the first healthy observation makes
	// every run recover at second zero -- including one whose target never came back. The first end-to-end
	// run against a real cluster reported exactly that, verdict Recovered at 0s, while its own trail showed
	// the failure arriving ten seconds later. envtest missed it because those specs start the target
	// already degraded, which is the one shape where the two readings agree.
	if !healthy {
		run.Status.ObservedUnhealthy = true
		return
	}
	if run.Status.ObservedUnhealthy && run.Status.RecoveredAtSeconds == nil {
		at := elapsed
		run.Status.RecoveredAtSeconds = &at
	}
}

// refuse ends the run without a verdict and says which part of the observation is missing.
func (r *WorkloadRunReconciler) refuse(ctx context.Context, run *platformv1.WorkloadRun, why string) (ctrl.Result, error) {
	run.Status.Phase = platformv1.WorkloadRunRefused
	run.Status.Reason = why
	// No verdict is cleared here, and there used to be a line that did with a comment explaining why.
	//
	// It was unreachable. A verdict is only ever set in the Complete branch, which is terminal, so there is
	// no path on which a refusal follows one -- the clear could not fire and the comment claimed a
	// protection that did not exist. A mutation that deleted the line failed nothing, which is how it was
	// found. The property it claimed is still worth holding, and the test holds it directly: a refused run
	// carries no verdict.
	run.Status.ObservedGeneration = run.Generation
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Refused", "run", run.Name, "reason", why)
	return ctrl.Result{}, nil
}

// observeTarget reads the target's phase verbatim.
//
// Unstructured on purpose: the two watchable kinds report a phase in the same place and this reads that
// place, rather than importing both types and teaching this controller their internals. What it must NOT
// do is normalise -- the target's own vocabulary is what goes in the trail.
func (r *WorkloadRunReconciler) observeTarget(ctx context.Context, run *platformv1.WorkloadRun) (string, bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   platformv1.GroupVersion.Group,
		Version: platformv1.GroupVersion.Version,
		Kind:    run.Spec.Target.Kind,
	})
	key := types.NamespacedName{Name: run.Spec.Target.Name, Namespace: run.Spec.Target.Namespace}
	if err := r.Get(ctx, key, u); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	phase, ok, err := unstructured.NestedString(u.Object, "status", "phase")
	if err != nil {
		return "", true, fmt.Errorf("read %s %q status.phase: %w", run.Spec.Target.Kind, key, err)
	}
	if !ok || phase == "" {
		// A target that has not reported yet is a real state and is recorded as one. Treating it as absent
		// would refuse every run that starts before its target's first reconcile.
		return "NoPhase", true, nil
	}
	return phase, true, nil
}

func targetKey(t platformv1.WorkloadRunTarget) string {
	if t.Namespace == "" {
		return t.Name
	}
	return t.Namespace + "/" + t.Name
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.WorkloadRun{}).
		Named("workloadrun").
		Complete(r)
}
