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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A WorkloadRun is one injected failure, watched, and an evidence trail that refuses to be written when it
// cannot be stood behind.
//
// The scenarios already existed as shell scripts with markdown pages beside them, and that is the problem
// this type solves. A page is written by hand after the fact: it drifts from what happened, nothing
// notices, and this repository has already had to retract claims that drifted that way. The trail here is
// assembled by a controller from what it observed, and when it could not observe, it says so instead of
// summarising.
//
// The discipline is queuelab's. A verdict is only emitted when the observation covers the window it claims
// to cover; a gap in the middle produces Refused rather than a pass computed from the parts that were
// watched. "The controller restarted and the target recovered while it was down" and "the target recovered"
// are different facts, and only one of them is evidence.

// WorkloadRunScenario names one injected failure.
//
// A closed set, because a free-form string would let a run describe an injection nobody can reproduce, and
// the evidence would then be about an event with no definition.
// The set is the scenarios this recorder can actually record, which is narrower than the set of scenarios
// this repository injects. A WorkloadRun watches ONE target's reported phase and judges a recovery in it, so
// a scenario whose observable is something else does not belong here however interesting it is.
//
// BackendFallback (hack/chaos-fr002b-backend-fallback.sh) is the one that was removed, and it is worth
// recording why rather than leaving a gap. Its observable is the gateway's backend_fallbacks_total moving,
// not a target recovering; and its injection is scaling the head backend to zero, which the
// InferenceDeployment controller reports as READY -- an intentional zero-replica state is Ready by design.
// A run watching that target would therefore see it healthy for the whole window and end Refused, forever,
// by construction. An enum value that can never produce a verdict is a promise this type does not keep.
// TestScalingABackendToZeroIsInvisibleToARecoveryWatcher pins the reason.
// +kubebuilder:validation:Enum=ServingPodKilled;DegradedNode
type WorkloadRunScenario string

const (
	// ScenarioServingPodKilled deletes a serving Pod under load; hack/chaos-fr002-serving-pod-killed.sh.
	ScenarioServingPodKilled WorkloadRunScenario = "ServingPodKilled"
	// ScenarioDegradedNode marks a node unhealthy so the taint path runs; hack/chaos-fr004-degraded-node.sh.
	//
	// It fits the recorder -- NodeHealth reports Ready, Pending and Quarantine, so a degradation and its
	// recovery are both visible -- but hack/m7-evidence-trail.sh does not drive it. The injection stops a
	// kubelet, which is disruptive to whatever else is on the cluster, and the script adopts a cluster it
	// does not own. Driving it needs a machine whose disruption nobody minds.
	ScenarioDegradedNode WorkloadRunScenario = "DegradedNode"
)

// WorkloadRunTarget is the object whose recovery the run is evidence about.
type WorkloadRunTarget struct {
	// kind is the target's kind. Only the kinds a scenario can actually act on are accepted.
	// +required
	// +kubebuilder:validation:Enum=InferenceDeployment;NodeHealth
	Kind string `json:"kind"`
	// name is the target's name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// namespace is the target's namespace; empty for cluster-scoped kinds.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WorkloadRunSpec declares what was injected, what is being watched, and what would count as recovery.
//
// The expectation is DECLARED BEFORE the run rather than read off the result, which is the same reason the
// benchmark harness freezes its manifest: a threshold chosen after seeing the number is not a threshold.
type WorkloadRunSpec struct {
	// scenario is the failure to inject. Immutable: a run is evidence about one injection, and editing it
	// would leave observations attributed to an event that did not happen.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="scenario is immutable"
	Scenario WorkloadRunScenario `json:"scenario"`
	// target is the object to watch.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target is immutable"
	Target WorkloadRunTarget `json:"target"`
	// observationWindowSeconds is how long to watch after the run starts.
	//
	// The window bounds the CLAIM as much as the work: a verdict says what happened inside it and nothing
	// about after, and a run that recovers one second late is a fail rather than a slow pass.
	// +required
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=3600
	ObservationWindowSeconds int32 `json:"observationWindowSeconds"`
	// recoversWithinSeconds is the deadline the target must be observed healthy by, measured from the run's
	// start. It must not exceed observationWindowSeconds, or the run would be asked to judge a deadline it
	// stopped watching before.
	// +required
	// +kubebuilder:validation:Minimum=1
	RecoversWithinSeconds int32 `json:"recoversWithinSeconds"`
}

// WorkloadRunPhase is where a run is in its lifecycle.
// +kubebuilder:validation:Enum=Pending;Observing;Complete;Refused
type WorkloadRunPhase string

const (
	// WorkloadRunPending means the run has not started observing.
	WorkloadRunPending WorkloadRunPhase = "Pending"
	// WorkloadRunObserving means the window is open and the trail is being assembled.
	WorkloadRunObserving WorkloadRunPhase = "Observing"
	// WorkloadRunComplete means the window closed over a covered observation and a verdict was reached.
	WorkloadRunComplete WorkloadRunPhase = "Complete"
	// WorkloadRunRefused means no verdict can be stood behind, and the reason says which part is missing.
	//
	// It is a terminal state and NOT a failure of the target. Conflating the two is what makes an evidence
	// trail worthless: "the platform did not recover" and "nobody was watching" have to be distinguishable
	// from the record alone.
	WorkloadRunRefused WorkloadRunPhase = "Refused"
)

// WorkloadRunVerdict is what the covered observation supports.
// +kubebuilder:validation:Enum=Recovered;NotRecovered
type WorkloadRunVerdict string

const (
	// VerdictRecovered means the target was observed healthy inside recoversWithinSeconds.
	VerdictRecovered WorkloadRunVerdict = "Recovered"
	// VerdictNotRecovered means the window closed with the target never observed healthy in time.
	VerdictNotRecovered WorkloadRunVerdict = "NotRecovered"
)

// WorkloadRunObservation is one thing the controller saw, stamped against the run's own start.
type WorkloadRunObservation struct {
	// elapsedSeconds is when it was seen, measured from startedAt rather than in wall-clock, so a trail can
	// be read without knowing when the run happened.
	// +required
	ElapsedSeconds int32 `json:"elapsedSeconds"`
	// state is what the target reported. Recorded verbatim rather than normalised: a controller that
	// rewrote the target's own vocabulary would be summarising, which is what this type exists to stop.
	// +required
	State string `json:"state"`
	// healthy is this controller's reading of state, kept beside it rather than replacing it.
	// +required
	Healthy bool `json:"healthy"`
}

// WorkloadRunStatus is the evidence trail.
type WorkloadRunStatus struct {
	// phase is the run's lifecycle position.
	// +optional
	Phase WorkloadRunPhase `json:"phase,omitempty"`
	// startedAt is when observation began. Every elapsedSeconds is relative to it.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// lastObservedAt is when the controller last actually looked.
	//
	// It exists so a GAP is detectable. Observations record only changes, so a trail with nothing in it for
	// twenty seconds is indistinguishable from a platform that was steady for twenty seconds -- unless the
	// controller also says when it last looked. A run whose gap exceeds what its poll interval allows is
	// refused rather than concluded, because "nobody was watching" and "nothing happened" are the two facts
	// an evidence trail exists to keep apart.
	// +optional
	LastObservedAt *metav1.Time `json:"lastObservedAt,omitempty"`
	// observations is the trail, in the order they were seen.
	//
	// Only CHANGES are appended. A trail that recorded every poll would say how often the controller ran
	// rather than what the platform did.
	// +optional
	// +listType=atomic
	Observations []WorkloadRunObservation `json:"observations,omitempty"`
	// verdict is set only in phase Complete. An empty verdict beside phase Refused is the point: there is
	// no answer, rather than an answer of "no".
	// +optional
	Verdict WorkloadRunVerdict `json:"verdict,omitempty"`
	// observedUnhealthy records that the target was seen in a non-healthy state at least once.
	//
	// Without it a recovery cannot be told from a target that was never broken. Every run starts with its
	// target healthy, so "first healthy observation" is second zero -- and a run whose target never came
	// back would report Recovered at 0s on the strength of the state it began in.
	// +optional
	ObservedUnhealthy bool `json:"observedUnhealthy,omitempty"`
	// recoveredAtSeconds is when the target was first observed healthy AFTER being observed unhealthy.
	// +optional
	RecoveredAtSeconds *int32 `json:"recoveredAtSeconds,omitempty"`
	// reason explains the phase, and for Refused it names which part of the observation is missing so the
	// reader knows whether to re-run or to fix something.
	// +optional
	Reason string `json:"reason,omitempty"`
	// observedGeneration is the spec generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// conditions carries the standard machine-readable summary.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Scenario",type=string,JSONPath=`.spec.scenario`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Verdict",type=string,JSONPath=`.status.verdict`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkloadRun is one injected failure and the evidence trail assembled while it was watched.
type WorkloadRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkloadRunSpec `json:"spec"`
	// +optional
	Status WorkloadRunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkloadRunList contains a list of WorkloadRun.
type WorkloadRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadRun{}, &WorkloadRunList{})
}
