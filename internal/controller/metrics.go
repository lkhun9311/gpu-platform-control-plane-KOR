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

// package 선언: 이 파일이 속한 패키지 이름이다.
//
// 같은 디렉터리(internal/controller)의 다른 파일들과 이름이 같아야 하며, 그래야 이 파일에서 선언하는 counter들을 nodehealth_controller.go, inferencedeployment_controller.go, gpuquotapolicy_controller.go가 import 없이 그대로 쓸 수 있다.
package controller

import (
	// prometheus: metric 타입(Counter, CounterVec)과 옵션 구조체(CounterOpts)를 제공하는 표준 클라이언트 라이브러리다.
	"github.com/prometheus/client_golang/prometheus"
	// promauto: metric을 만들면서 등록부에 등록까지 한 번에 해 주는 보조 패키지다.
	//
	// prometheus.NewCounterVec을 만든 뒤 registry.MustRegister를 따로 부르는 두 단계를 한 줄로 줄여 준다.
	"github.com/prometheus/client_golang/prometheus/promauto"

	// metrics: controller-runtime의 전역 metric 등록부이며, 매니저가 이미 띄우고 있는 /metrics 엔드포인트가 바로 이 등록부를 노출한다.
	//
	// 여기에 등록하면 컨트롤러가 기본으로 내는 controller_runtime_reconcile_total 같은 시계열과 같은 엔드포인트, 같은 스크레이프 설정으로 나가므로 별도의 서버나 포트를 새로 열 필요가 없다.
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metricPrefix: 이 패키지가 내보내는 모든 시계열의 공통 접두사다.
//
// controller-runtime이 같은 등록부에 자동으로 얹는 controller_runtime_* 계열의 built-in metric과 이름이 겹치지 않게 구분해 준다.
const metricPrefix = "gpuplatform_"

// 아래 세 counter는 controller-runtime이 기본 제공하는 metric으로는 전혀 알 수 없는, 이 프로젝트 고유의 판단 결과를 센다.
//
// controller_runtime_reconcile_total과 controller_runtime_reconcile_time_seconds는 "재조정이 몇 번 돌았고 얼마나 걸렸는가"만 말해 줄 뿐, 그 재조정이 실제로 무엇을 결정했는지는 말해 주지 않는다.
//
// 예컨대 재조정이 100번 성공했어도 그중 실제로 taint를 걸거나 뗀 횟수, InferenceDeployment가 Degraded로 넘어간 횟수, ResourceQuota drift를 고쳐 쓴 횟수는 별개의 정보이며 운영자가 알림과 대시보드를 구성하려면 이 도메인 수치가 따로 필요하다.
//
// 셋 다 Gauge가 아니라 Counter로 둔 이유도 같은 맥락이다.
//
// Gauge는 프로세스가 재시작되면 값이 사라지거나 임의로 다시 설정될 수 있어 "그동안 몇 번 있었는가"라는 누적 질문에 취약하다.
//
// Counter는 재시작 후 0에서 다시 오르더라도 Prometheus의 rate()/increase() 함수가 그 리셋을 스스로 감지해 처리하므로, 알림 규칙이 재시작 여부와 무관하게 "최근 N분간 몇 번 발생했는가"를 안정적으로 answer할 수 있다.
var (
	// nodeHealthTaintTotal: 노드 격리 taint의 부여/해제 전환 횟수를 action 라벨로 나누어 센다.
	//
	// action="applied"는 not-ready 노드를 quarantine으로 판정해 taint를 새로 건 경우이고, action="removed"는 노드가 회복해 taint를 뗀 경우다.
	//
	// 두 값을 하나의 CounterVec으로 묶은 이유: 격리와 해제는 같은 taint 리소스에 대한 대칭적인 두 전환이라 라벨 하나로 함께 조회하는 편이 "지금 격리 중인 노드의 순증감"을 계산하기 쉽다.
	nodeHealthTaintTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "nodehealth_taint_total",
			Help: "Number of unhealthy-node taint transitions, labeled by action.",
		},
		[]string{"action"},
	)

	// inferenceDeploymentDegradedTotal: InferenceDeployment가 Degraded phase로 들어간 횟수를 reason 라벨로 나누어 센다.
	//
	// reason 값은 markDegraded 호출부가 넘기는 문자열 그대로이며, Available condition에 남는 결정적 실패 사유(예: 소유권 충돌)와 동일하다.
	//
	// 같은 문자열을 status와 metric 양쪽에 쓰는 이유: 운영자가 kubectl로 본 이유와 대시보드에서 본 이유가 어긋나면 원인 추적이 두 배로 늘어나므로, 하나의 상수를 두 곳에서 공유해 그 문제를 원천 차단한다.
	inferenceDeploymentDegradedTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "inferencedeployment_degraded_total",
			Help: "Number of times an InferenceDeployment entered the Degraded phase, labeled by reason.",
		},
		[]string{"reason"},
	)

	// gpuQuotaPolicyDriftCorrectedTotal: 정책이 관리하는 ResourceQuota가 spec과 어긋난 것을 발견해 되돌려 쓴 횟수를 센다.
	//
	// 라벨이 없는 단순 Counter인 이유: drift 교정은 정책별로 구분해 봐야 할 만큼 값이 다양하지 않고, "이 클러스터 전체에서 quota가 얼마나 자주 이탈해 다시 고쳐지는가"라는 한 가지 총량 질문에만 답하면 충분하다.
	//
	// 이 값이 계속 늘어난다면 정책과 무관하게 ResourceQuota를 직접 고치는 외부 주체(사람의 kubectl edit, 다른 컨트롤러 등)가 있다는 신호이며, 그 자체가 조사해야 할 이상 징후다.
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

	// mlTrainingJobAdmitToRunningSeconds is how long a tenant waited between having quota and using it.
	//
	// A histogram rather than a counter because the interesting thing is the tail: a cohort where reclaim
	// usually costs two seconds and occasionally costs two minutes is not the same platform as one where it
	// always costs twenty, and a mean hides which one you are running.
	//
	// The buckets are chosen around the two numbers that already exist. queuelab measured 2.180 s when a
	// preempted borrower honoured SIGTERM and 31.213 s when it ignored it, and the admission webhook caps
	// terminationGracePeriodSeconds at 120. So the range that matters runs from about a second to about two
	// minutes, and the buckets are placed to separate those three regimes rather than spread evenly.
	mlTrainingJobAdmitToRunningSeconds = promauto.With(metrics.Registry).NewHistogram(
		prometheus.HistogramOpts{
			Name:    metricPrefix + "mltrainingjob_admit_to_running_seconds",
			Help:    "Seconds between Kueue admitting a training job and this controller observing it running.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 240},
		},
	)

	// mlTrainingJobAdmitToRunningUnobservedTotal counts the jobs whose wait could not be measured.
	//
	// It exists because a quiet histogram has two causes that look identical: nothing ran, or everything ran
	// and nothing was watched. A platform that cannot tell those apart reports "reclaim is fast" from an
	// empty series, which is the failure this repository keeps finding in its own instruments.
	mlTrainingJobAdmitToRunningUnobservedTotal = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "mltrainingjob_admit_to_running_unobserved_total",
			Help: "Training jobs seen running whose admission-to-running window this controller did not observe.",
		},
		[]string{"reason"},
	)
)
