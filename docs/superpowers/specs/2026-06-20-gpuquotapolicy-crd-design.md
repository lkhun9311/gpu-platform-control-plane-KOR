# GPUQuotaPolicy CRD 설계 (M1)

- 날짜: 2026-06-20
- 마일스톤: M1 (프로젝트 스켈레톤 + 4개 CRD 정의, envtest 검증)
- 대상: 4개 CRD 중 두 번째 — `GPUQuotaPolicy`
- 작성자: lkhun9311

## 배경

`gpu-platform-control-plane`은 GPU를 공유 플랫폼 리소스로 다루는 Kubernetes 네이티브 컨트롤
플레인이다. README의 영역 표에서 멀티테넌트 quota 영역은 다음과 같이 정의된다:

> 테넌트별 quota와 격리 정책을 `GPUQuotaPolicy`에서 namespace 객체로 동기화한다.

M1의 범위는 **타입 정의 + envtest 검증**까지다. 실제 namespace 객체(ResourceQuota 등)
동기화 로직은 M3 리컨실러의 몫이다. 본 설계는 이미 머지된 `NodeHealth` CRD와 동일한
altitude(현실적 spec 필드 + phase enum/conditions/observedGeneration status, 시뮬레이션
부분은 정직성 주석)를 따른다.

## 목표 / 비목표

**목표**
- 테넌트별 GPU quota 정책을 선언하는 `GPUQuotaPolicy` CRD 타입 정의
- `NodeHealth`와 일관된 status 구조(phase enum, conditions, observedGeneration)
- 샘플 매니페스트 + envtest 테스트로 검증

**비목표 (M3 이후)**
- ResourceQuota / LimitRange 등 namespace 객체 동기화
- 실제 GPU 사용량 집계 / quota 초과 차단
- 테넌트 단위 사용량 롤업

## API 설계

### Scope
`Cluster`-scoped. 멀티테넌트 quota는 클러스터 관리자가 정의하는 정책이므로 클러스터
스코프가 자연스럽고, 기존 `NodeHealth`(Cluster-scoped)와도 일관된다.

### Group / Version / Kind
- group: `platform` (domain `lkhun9311.github.io`)
- version: `v1`
- kind: `GPUQuotaPolicy`

### Spec

```go
// GPUQuotaPolicySpec defines the desired state of GPUQuotaPolicy.
type GPUQuotaPolicySpec struct {
    // tenant is the logical tenant (team/org) this policy applies to.
    // A tenant may own multiple namespaces, so this is distinct from targetNamespace.
    // +required
    Tenant string `json:"tenant"`

    // targetNamespace is the namespace into which quota objects are synced.
    // +required
    TargetNamespace string `json:"targetNamespace"`

    // gpuClass scopes the quota to a GPU class (e.g. "l40s"). Empty means all classes.
    // Locally this is backed by simulated capacity (see the dev runbook).
    // +optional
    GPUClass string `json:"gpuClass,omitempty"`

    // limits is the quota ceiling for this tenant in the target namespace.
    // +required
    Limits GPUQuotaLimits `json:"limits"`
}

// GPUQuotaLimits is the quota ceiling for a tenant.
type GPUQuotaLimits struct {
    // gpuCount is the maximum number of GPUs (nvidia.com/gpu) allowed.
    // Locally this is backed by simulated capacity, not real hardware.
    // +kubebuilder:validation:Minimum=0
    // +required
    GPUCount int32 `json:"gpuCount"`
}
```

**tenant + targetNamespace 분리 근거**: 논리적 테넌트(과금/소유 주체)와 물리적
namespace(격리 단위)를 독립적으로 모델링한다. 한 테넌트가 dev/prod처럼 여러 namespace를
가질 때 정책을 각각 만들되 동일 `tenant`로 묶어, M3에서 테넌트 기준 집계가 가능하다.
M1에서는 필드만 분리해 두고 집계/sync는 구현하지 않는다.

### Status

`NodeHealth`와 동일 구조를 따른다.

```go
// GPUQuotaPolicyStatus defines the observed state of GPUQuotaPolicy.
type GPUQuotaPolicyStatus struct {
    // phase is the high-level sync state of the policy.
    // +kubebuilder:validation:Enum=Pending;Synced;Degraded
    // +optional
    Phase string `json:"phase,omitempty"`

    // observedGeneration is the most recent generation observed by the controller.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // lastTransitionTime is the time the phase last changed.
    // +optional
    LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

    // conditions represent the current state of the GPUQuotaPolicy resource.
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

phase 의미:
- `Pending`: 아직 동기화되지 않음 (M1 리컨실러는 항상 여기 머무름)
- `Synced`: namespace 객체가 정책과 일치 (M3)
- `Degraded`: 동기화 실패 또는 드리프트 (M3)

### 마커
- `// +kubebuilder:object:root=true`
- `// +kubebuilder:subresource:status`
- `// +kubebuilder:resource:scope=Cluster`
- printcolumn:
  - `Tenant` ← `.spec.tenant`
  - `Namespace` ← `.spec.targetNamespace`
  - `Phase` ← `.status.phase`
  - `Age` ← `.metadata.creationTimestamp`

## 리컨실러

M1에서는 `NodeHealth`와 동일하게 빈 리컨실러로 둔다 — 요청을 로그로만 남기고,
"quota sync / 드리프트 복구는 M3"임을 주석으로 명시한다. RBAC 마커는 kubebuilder가
생성하는 기본값(get/list/watch/create/update/patch/delete + status/finalizers)을 유지한다.

## 작업 절차 (AGENTS.md 규칙 준수)

1. `kubebuilder create api --group platform --version v1 --kind GPUQuotaPolicy`
   (resource + controller 스캐폴드, 수동 파일 생성 금지)
2. 생성된 `api/v1/gpuquotapolicy_types.go`를 위 스키마로 편집
3. `internal/controller/gpuquotapolicy_controller.go`에 M3 주석 추가 (NodeHealth 패턴)
4. `make manifests && make generate` (CRD/RBAC/DeepCopy 재생성)
5. `config/samples/platform_v1_gpuquotapolicy.yaml` 샘플 작성 + kustomization 등록
6. envtest 테스트 작성 (`gpuquotapolicy_controller_test.go`) → `make test`
7. `make lint-fix && make test`로 검증

## 검증 (M1 완료 기준)

- `make manifests generate` 후 트리 clean (생성물 정상)
- `make test`(envtest) 통과 — CRD 생성/조회 및 status 서브리소스 업데이트 검증
- `make lint` 통과
- `kubectl apply` 가능한 샘플 매니페스트 존재

## 범위 / 단위

본 설계는 **`GPUQuotaPolicy` 단일 CRD**만 다룬다. 나머지 두 CRD
(`InferenceDeployment`, `MLTrainingJob`)는 각각 별도 브랜치/PR/설계로 진행한다.
브랜치: `feat/m1-gpuquotapolicy-crd`.