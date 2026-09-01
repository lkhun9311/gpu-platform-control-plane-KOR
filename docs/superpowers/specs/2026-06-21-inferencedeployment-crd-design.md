# InferenceDeployment CRD 설계 (M1)

- 날짜: 2026-06-21
- 마일스톤: M1 (프로젝트 스켈레톤 + 4개 CRD 정의, envtest 검증)
- 대상: 4개 CRD 중 세 번째 — `InferenceDeployment`
- 작성자: lkhun9311

## 배경

README 영역 표에서 추론 서빙 영역은 다음과 같이 정의된다:

> 서빙 워크로드를 `InferenceDeployment`로 선언적으로 관리한다.

기술 스택에는 KEDA(오토스케일링)가 포함된다. M1 범위는 **타입 정의 + envtest 검증**까지다.
실제 Deployment/Service 생성과 오토스케일링은 M4 리컨실러의 몫이다. 본 설계는 기존
`NodeHealth`/`GPUQuotaPolicy` CRD와 동일한 altitude(현실적 spec 필드 + phase enum/conditions/
observedGeneration status, 시뮬레이션 부분은 정직성 주석)를 따른다.

## 목표 / 비목표

**목표**
- 서빙 워크로드를 선언하는 `InferenceDeployment` CRD 타입 정의 (Namespaced)
- `NodeHealth`/`GPUQuotaPolicy`와 일관된 status 구조 + 워크로드 관측 필드(readyReplicas)
- 샘플 매니페스트 + envtest 테스트로 검증

**비목표 (M4 이후)**
- Deployment / Service 등 네이티브 객체 생성
- KEDA 연동 오토스케일링 (min/max replicas)
- 모델 가중치 로딩 / 실제 GPU 서빙

## API 설계

### Scope
`Namespaced`. 서빙 워크로드는 테넌트 namespace에 속하는 자원이며, M4에서 동일 namespace에
Deployment/Service를 생성한다. 기존 두 CRD(Cluster-scoped)와 달리 워크로드 성격에 맞춰
Namespaced로 둔다.

### Group / Version / Kind
- group: `platform` (domain `lkhun9311.github.io`)
- version: `v1`
- kind: `InferenceDeployment`

### Spec

```go
// InferenceDeploymentSpec defines the desired state of InferenceDeployment.
type InferenceDeploymentSpec struct {
	// model is the model to serve.
	// +required
	Model InferenceModel `json:"model"`

	// image is the serving runtime container image (e.g. "vllm/vllm-openai:v0.6.0").
	// +required
	Image string `json:"image"`

	// gpuClass is the illustrative GPU class (e.g. "l40s").
	// Locally this is backed by simulated capacity (see the dev runbook).
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`

	// gpuCount is the number of GPUs (nvidia.com/gpu) per replica.
	// Locally this is backed by simulated capacity, not real hardware.
	// +kubebuilder:validation:Minimum=0
	// +required
	GPUCount int32 `json:"gpuCount"`

	// replicas is the fixed number of serving replicas. Autoscaling lands in M4.
	// +kubebuilder:validation:Minimum=0
	// +required
	Replicas int32 `json:"replicas"`

	// port is the serving container port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`
}

// InferenceModel identifies the model to serve.
type InferenceModel struct {
	// name is the logical model name.
	// +required
	Name string `json:"name"`

	// storageUri is where the model weights live (e.g. "s3://bucket/model", "pvc://claim/path").
	// +required
	StorageURI string `json:"storageUri"`
}
```

### Status

```go
// InferenceDeploymentStatus defines the observed state of InferenceDeployment.
type InferenceDeploymentStatus struct {
	// phase is the high-level serving state.
	// +kubebuilder:validation:Enum=Pending;Progressing;Ready;Degraded
	// +optional
	Phase string `json:"phase,omitempty"`

	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// readyReplicas is the observed number of ready serving replicas (populated in M4).
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// lastTransitionTime is the time the phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// conditions represent the current state of the InferenceDeployment resource.
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

phase 의미:
- `Pending`: 아직 처리되지 않음 (M1 리컨실러는 항상 여기 머무름)
- `Progressing`: Deployment 롤아웃 중 (M4)
- `Ready`: 요청 replicas가 모두 ready (M4)
- `Degraded`: 롤아웃 실패 또는 드리프트 (M4)

### 마커
- `// +kubebuilder:object:root=true`
- `// +kubebuilder:subresource:status`
- scope는 Namespaced(기본값) — 별도 scope 마커 없음
- printcolumn:
  - `Model` ← `.spec.model.name`
  - `Phase` ← `.status.phase`
  - `Ready` ← `.status.readyReplicas`
  - `Age` ← `.metadata.creationTimestamp`

## 리컨실러

M1에서는 `NodeHealth`/`GPUQuotaPolicy`와 동일하게 빈 리컨실러로 둔다 — 요청을 로그로만
남기고, "Deployment/Service 생성과 오토스케일링은 M4"임을 주석으로 명시한다. RBAC 마커는
kubebuilder 기본값을 유지한다.

## 작업 절차 (AGENTS.md 규칙 준수)

1. `kubebuilder create api --group platform --version v1 --kind InferenceDeployment --resource --controller`
2. 생성된 `api/v1/inferencedeployment_types.go`를 위 스키마로 편집
3. `internal/controller/inferencedeployment_controller.go`에 M4 주석 추가
4. `make manifests generate`
5. `config/samples/platform_v1_inferencedeployment.yaml` 샘플 작성 + kustomization 등록 확인
6. envtest 테스트 작성 → `make test`
7. `make lint` 검증

## 검증 (M1 완료 기준)

- `make manifests generate` 후 트리 clean
- `make test`(envtest) 통과 — Namespaced CRD 생성/조회 및 status 서브리소스 업데이트 검증
- `make lint` 통과
- `kubectl apply` 가능한 샘플 매니페스트 존재

## 범위 / 단위 / 브랜치

본 설계는 **`InferenceDeployment` 단일 CRD**만 다룬다. 마지막 CRD `MLTrainingJob`은 별도
브랜치/PR/설계로 진행한다. 브랜치: `feat/m1-inferencedeployment-crd`, base `milestone/m1-skeleton`.

> 참고: GPUQuotaPolicy PR(#4)이 아직 `milestone/m1-skeleton`에 머지되지 않았다. 이 CRD를
> `milestone/m1-skeleton`에서 분기하면 `PROJECT`·`cmd/main.go`·`config/*/kustomization.yaml`
> 같은 스캐폴드 공유 파일에서 사소한(additive) 머지가 발생할 수 있다. PR #4를 먼저 머지한 뒤
> 갱신된 base에서 분기하면 충돌 없이 진행된다 — 구현 시작 전 머지 상태를 확인한다.