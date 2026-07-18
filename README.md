# gpu-platform-control-plane 한국어판

GPU를 단순한 장비가 아니라 Kubernetes 플랫폼 리소스로 다루는 컨트롤 플레인입니다.

## 한 줄 요약

이 프로젝트는 GPU 노드 상태, 테넌트별 쿼터, 추론 서빙, 학습 잡 입장 제어, 관측성 증거를 하나의 Kubernetes-native 컨트롤 플레인으로 묶습니다. 포트폴리오 관점에서는 "GPU 플랫폼을 운영자가 어떻게 안전하게 공유 자원으로 노출할 것인가"를 보여주는 프로젝트입니다.

## 핵심 범위

| 영역 | 설명 |
| --- | --- |
| GPU 노드 상태 | `NodeHealth` CR로 GPU 노드의 준비 상태를 표현하고, 비정상 노드는 스케줄링에서 제외합니다. |
| 멀티테넌트 쿼터 | `GPUQuotaPolicy`를 기준으로 namespace별 `ResourceQuota`와 격리 정책을 동기화합니다. |
| 추론 서빙 | `InferenceDeployment`로 모델 서빙 Deployment와 Service를 선언적으로 관리합니다. |
| 성능 격리 | GPU 공유 상황에서 noisy neighbor가 p99 지연시간에 주는 영향을 측정하고 완화합니다. |
| 게이트웨이 | API key로 테넌트를 식별하고, 토큰 버킷 기반 rate limit과 모델 라우팅을 수행합니다. |
| 학습 입장 제어 | `MLTrainingJob`을 Kueue 기반 `Job`/`Workload` 흐름으로 연결합니다. |
| 운영 증거 | 메트릭, 상태, 이벤트, 운영 로그를 문서와 ledger로 남겨 설계 판단을 검증합니다. |

## 아키텍처

컨트롤 플레인은 CRD를 소유하고, 각 CR을 Kubernetes 기본 리소스로 reconcile합니다. 데이터 플레인은 Deployment, Service, Job, ResourceQuota처럼 익숙한 Kubernetes 오브젝트로 유지하며 owner reference로 수명주기를 관리합니다.

```text
사용자/플랫폼 팀
  -> GPUQuotaPolicy / NodeHealth / InferenceDeployment / MLTrainingJob
  -> Controller Reconcile
  -> ResourceQuota / Deployment / Service / Job / Kueue Workload
  -> 메트릭, 상태, 이벤트, 운영 증거
```

## 마일스톤 상태

| 마일스톤 | 범위 | 상태 |
| --- | --- | --- |
| M1 | 프로젝트 골격과 핵심 CRD 정의, envtest 검증 | 완료 |
| M2 | idempotent reconcile, finalizer, drift recovery | 완료 |
| M3 | 비정상 GPU 노드 taint, 테넌트별 ResourceQuota 동기화 | 완료 |
| M4-a | `InferenceDeployment` 기반 추론 워크로드 관리 | 완료 |
| M4-b | 테넌트 인식 서빙 게이트웨이: API key → tenant, 토큰 버킷 → 429, 모델 라우팅, 프록시, 메트릭 | 완료 |
| M5-a | AWS 호스팅: Terraform(state bootstrap, EKS, 노드 그룹), GitHub Actions CI(OIDC → ECR), Argo CD GitOps, EKS에 오퍼레이터 배포, 경량 관측성(아직 GPU 없음) | 설계 완료(v3.1) |
| M5-b | 그 인프라 위의 real-GPU 플래그십: GPU 노드 그룹(On-Demand, ephemeral), `GpuSharingBenchmark`와 KV-cache 인식 입장 가드, noisy neighbor p99 A/B 측정(핵심 기능) | 설계 완료 |
| M5-c | 심화: 비용/공정성 프론티어(가드 임계값 3개 이상)와 공유 모드 매트릭스(exclusive / time-slicing / MPS). M5-b 증거를 강화하며 새 기능은 없다 | 계획 |
| M5-d | 측정 수치를 담은 기술 문서(M5-c 이후 공개) | 계획 |
| M6 | 학습 입장 제어(스트레치에서 승격): `MLTrainingJob` → Job과 Kueue Workload, 2-테넌트 공정 공유, kind에서 preemption 증거 확보. 학습 쿼터는 Kueue가 소유한다 | 계획 |
| M7 | 실패 시나리오 주입과 운영 증거 기록(`WorkloadRun`) | 스케치 |

## 기술 스택

- Go, controller-runtime, Kubebuilder
- kind, envtest, Kubernetes CRD/RBAC/Kustomize
- Kueue, KEDA, Prometheus 계열 관측성 도구

## 로컬 개발

```bash
kind create cluster --config hack/kind-config.yaml
make build
make test
```

> **이 판본에서는 `make manifests`를 실행하지 마세요.**
>
> controller-gen은 `api/v1/*_types.go`의 필드 doc 주석을 CRD 스키마의 `description`으로 그대로 복사합니다.
> 이 판본의 주석에는 Go 문법 설명이 들어 있어서, 재생성하면 그 설명이 클러스터에 적용되는 CRD와 `kubectl explain` 출력에 그대로 실리고 CRD 크기도 약 1.5배(33KB → 53KB)가 됩니다.
> 그래서 `config/crd/bases/`의 CRD는 영어판에서 생성한 것을 그대로 두어, 두 판본이 동일한 CRD를 배포하도록 유지합니다.
> 스키마를 바꿔야 한다면 영어판에서 `make manifests`를 돌린 뒤 그 결과를 이 판본으로 복사하세요.
>
> 같은 이유로 `api/v1/zz_generated.deepcopy.go`에는 한국어 주석을 달지 않습니다. `make generate`가 덮어쓰기 때문입니다.

kind 환경에서는 실제 GPU 없이도 스케줄링과 쿼터 enforcement 흐름을 검증할 수 있도록 노드 상태에 가짜 GPU capacity를 패치합니다.

```bash
kubectl patch node platform-worker --subresource=status --type=json \
  -p='[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"4"},
       {"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"4"}]'
```

## 디렉터리 구조

```text
api/      CRD 타입 정의
cmd/      컨트롤러와 게이트웨이 엔트리포인트
config/   CRD, RBAC, manager, sample manifest
docs/     설계 문서와 포트폴리오 설명
internal/ 컨트롤러와 게이트웨이 구현
test/     e2e 테스트 골격
```

## 라이선스

Apache 2.0
