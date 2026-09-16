# MLTrainingJob CRD 설계 (M1)

- 날짜: 2026-06-21
- 마일스톤: M1 (프로젝트 스켈레톤 + 4개 CRD 정의, envtest 검증)
- 대상: 4개 CRD 중 네 번째(마지막) — `MLTrainingJob`
- 작성자: lkhun9311

## 배경

README 영역 표에서 학습 admission 영역은 다음과 같이 정의된다.

> `MLTrainingJob`을 Kueue로 admit되는 `batch/v1` Job으로 변환한다.

이 프로젝트는 스케줄러를 재구현하지 않는다. Kueue를 admission 엔진으로 쓰고, `MLTrainingJob`
추상화와 상태 변환만 그 위에 얹는다. M1 범위는 타입 정의와 envtest 검증까지다. 실제 `batch/v1`
Job 생성과 Kueue 연동, 상태 변환은 M5 리컨실러의 몫이다. 본 설계는 앞선 세 CRD와 동일한
altitude(현실적 spec 필드, phase enum과 conditions, observedGeneration을 둔 status, 시뮬레이션
부분은 정직성 주석)를 따른다.

## 목표 / 비목표

목표

- Kueue로 admit되는 학습 작업을 선언하는 `MLTrainingJob` CRD 타입 정의 (Namespaced)
- 앞선 CRD들과 일관된 status 구조 (phase enum, observedGeneration, conditions)
- 샘플 매니페스트와 envtest 테스트로 검증

비목표 (M5 이후)

- `batch/v1` Job 생성과 Kueue LocalQueue 연동
- Job 진행 상태를 phase로 변환하는 로직
- 실제 GPU 학습 실행

## API 설계

### Scope

`Namespaced`. 학습 Job과 Kueue LocalQueue가 모두 namespace 자원이므로, 같은 namespace에서
다루는 것이 자연스럽다. InferenceDeployment와 같은 결정이다.

### Group / Version / Kind

- group: `platform` (domain `lkhun9311.github.io`)
- version: `v1`
- kind: `MLTrainingJob`

### Spec

```go
// MLTrainingJobSpec defines the desired state of MLTrainingJob.
type MLTrainingJobSpec struct {
	// queue is the Kueue LocalQueue name (same namespace) this job is admitted through.
	// +required
	Queue string `json:"queue"`

	// image is the training container image.
	// +required
	Image string `json:"image"`

	// command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// gpuClass is the illustrative GPU class (e.g. "l40s").
	// Locally this is backed by simulated capacity (see the dev runbook).
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`

	// gpuCount is the number of GPUs (nvidia.com/gpu) per pod.
	// Locally this is backed by simulated capacity, not real hardware.
	// +kubebuilder:validation:Minimum=0
	// +required
	GPUCount int32 `json:"gpuCount"`

	// parallelism is the batch/v1 Job parallelism (concurrent pods).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Parallelism int32 `json:"parallelism,omitempty"`

	// completions is the batch/v1 Job completions (successful pods required).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Completions int32 `json:"completions,omitempty"`
}
```

`queue`는 같은 namespace의 Kueue LocalQueue 이름을 가리키는 단순 문자열로 둔다. M5에서 이
이름으로 Job에 `kueue.x-k8s.io/queue-name` 라벨을 붙여 admission을 태운다.

### Status

앞선 CRD들과 동일한 기본 구조만 둔다. 만들어진 Job 이름 같은 관측 필드는 실제 Job을 생성하는
M5에서 로직과 함께 추가한다.

```go
// MLTrainingJobStatus defines the observed state of MLTrainingJob.
type MLTrainingJobStatus struct {
	// phase tracks the Kueue admission and run lifecycle.
	// +kubebuilder:validation:Enum=Pending;Admitted;Running;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastTransitionTime is the time the phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// conditions represent the current state of the MLTrainingJob resource.
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

phase 의미

- `Pending`: 아직 처리되지 않음 (M1 리컨실러는 항상 여기 머무름)
- `Admitted`: Kueue가 워크로드를 admit함 (M5)
- `Running`: Job 파드가 실행 중 (M5)
- `Succeeded`: 요청 completions를 채움 (M5)
- `Failed`: Job 실패 (M5)

### 마커

- `// +kubebuilder:object:root=true`
- `// +kubebuilder:subresource:status`
- scope는 Namespaced(기본값) — 별도 scope 마커 없음
- printcolumn
  - `Queue` ← `.spec.queue`
  - `Phase` ← `.status.phase`
  - `Age` ← `.metadata.creationTimestamp`

## 리컨실러

앞선 CRD들과 동일하게 빈 리컨실러로 둔다. 요청을 로그로만 남기고, Job 생성과 Kueue 연동은
M5라는 점을 주석으로 명시한다. RBAC 마커는 kubebuilder 기본값을 유지한다.

## 작업 절차 (AGENTS.md 규칙 준수)

1. `kubebuilder create api --group platform --version v1 --kind MLTrainingJob --resource --controller`
2. 생성된 `api/v1/mltrainingjob_types.go`를 위 스키마로 편집
3. `internal/controller/mltrainingjob_controller.go`에 M5 주석 추가
4. `make manifests generate`
5. `config/samples/platform_v1_mltrainingjob.yaml` 샘플 작성, kustomization 등록 확인
6. envtest 테스트 작성 후 `make test`
7. `make lint` 검증

## 검증 (M1 완료 기준)

- `make manifests generate` 후 트리 clean
- `make test`(envtest) 통과 — Namespaced CRD 생성/조회와 status 서브리소스 업데이트 검증
- `make lint` 통과
- `kubectl apply` 가능한 샘플 매니페스트 존재

## 범위 / 단위 / 브랜치

본 설계는 `MLTrainingJob` 단일 CRD만 다룬다. 이로써 M1의 4개 CRD가 모두 완성된다. 브랜치는
`feat/m1-mltrainingjob-crd`, base는 `milestone/m1-skeleton`이다. PR #5(InferenceDeployment)가
머지된 뒤 갱신된 base에서 분기하면 `PROJECT`, `cmd/main.go`, kustomization 같은 공유 파일에서
충돌 없이 진행된다. 구현 시작 전 #5 머지 상태를 확인한다.