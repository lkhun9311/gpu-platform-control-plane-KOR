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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	// metrics is controller-runtime's global registry, already served on the manager's /metrics endpoint.
	//
	// Registering here puts these series on the same endpoint as the built-in controller_runtime_* metrics, with no separate wiring.
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metricPrefix namespaces every series this package exposes.
//
// It keeps these domain metrics apart from the built-in controller_runtime_* series on the same registry.
const metricPrefix = "gpuplatform_"

// These three counters capture domain transitions the built-in controller-runtime metrics cannot express:
// reconcile counts and durations say a controller ran, not what it decided.
var (
	// nodeHealthTaintTotal counts unhealthy-node taint transitions, by action.
	//
	// action is "applied" when a node is quarantined and "removed" when it recovers.
	nodeHealthTaintTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "nodehealth_taint_total",
			Help: "Number of unhealthy-node taint transitions, labeled by action.",
		},
		[]string{"action"},
	)

	// inferenceDeploymentDegradedTotal counts entries into the Degraded phase, by reason.
	//
	// The reason label is the same deterministic-failure reason recorded on the Available condition.
	inferenceDeploymentDegradedTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "inferencedeployment_degraded_total",
			Help: "Number of times an InferenceDeployment entered the Degraded phase, labeled by reason.",
		},
		[]string{"reason"},
	)

	// gpuQuotaPolicyDriftCorrectedTotal counts ResourceQuota drift corrections.
	//
	// It increments each time a policy's synced ResourceQuota is found to differ from spec and is written back.
	gpuQuotaPolicyDriftCorrectedTotal = promauto.With(metrics.Registry).NewCounter(
		prometheus.CounterOpts{
			Name: metricPrefix + "gpuquotapolicy_drift_corrected_total",
			Help: "Number of times a drifted ResourceQuota was corrected back to the policy spec.",
		},
	)

	// mlTrainingJobFailedTotal counts entries into the Failed phase, by reason.
	//
	// The reason label is the same deterministic-failure reason recorded on the JobSynced condition.
	mlTrainingJobFailedTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "mltrainingjob_failed_total",
			Help: "Number of times an MLTrainingJob entered the Failed phase, labeled by reason.",
		},
		[]string{"reason"},
	)

	// mlTrainingJobPhaseTotal counts MLTrainingJob phase transitions, by phase.
	//
	// It increments each time a phase actually changes, right after the status update succeeds.
	mlTrainingJobPhaseTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "mltrainingjob_phase_total",
			Help: "Number of MLTrainingJob phase transitions, labeled by phase.",
		},
		[]string{"phase"},
	)
)
