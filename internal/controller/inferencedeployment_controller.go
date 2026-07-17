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
// 같은 디렉터리(internal/controller)의 모든 .go 파일은 반드시 같은 package 이름(controller)을 가져야 한다.
// 그래야 suite_test.go의 k8sClient 같은 것을 이 패키지의 다른 파일에서 import 없이 바로 쓸 수 있다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	// 쿠버네티스 클라이언트 호출은 요청 중단을 위해 전부 ctx를 첫 인자로 받는다.
	"context"
	// fmt: 문자열 포맷팅 표준 패키지이며, 여기서는 fmt.Errorf로 에러에 맥락을 덧붙이는 데 쓴다.
	"fmt"

	// appsv1: Deployment 등 apps/v1 API 그룹의 쿠버네티스 내장 타입들이다.
	// import 경로 앞의 appsv1은 별칭(alias)이며, 원래 패키지 이름은 v1이지만 다른 v1들과 구분하려고 이렇게 부른다.
	appsv1 "k8s.io/api/apps/v1"
	// corev1: Service, Pod, Container 등 core/v1 API 그룹의 타입들이다.
	corev1 "k8s.io/api/core/v1"
	// equality: 쿠버네티스 객체를 "의미적으로(semantically)" 비교하는 도구다.
	// 아래 equality.Semantic.DeepEqual에서 status 변경 여부를 판정하는 데 쓴다.
	"k8s.io/apimachinery/pkg/api/equality"
	// apierrors: API 서버가 돌려준 에러의 종류를 판별하는 헬퍼들이다(예: IsNotFound).
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	// meta: condition 슬라이스를 다루는 표준 헬퍼다(아래 meta.SetStatusCondition).
	"k8s.io/apimachinery/pkg/api/meta"
	// resource: CPU/메모리/GPU 같은 자원 수량(Quantity)을 표현하는 타입이다.
	"k8s.io/apimachinery/pkg/api/resource"
	// metav1: ObjectMeta, LabelSelector, Condition 등 모든 쿠버네티스 객체가 공유하는 메타데이터 타입들이다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// runtime: Scheme(고 타입 <-> API 그룹/버전 매핑표) 타입이 들어 있다.
	"k8s.io/apimachinery/pkg/runtime"
	// types: NamespacedName(네임스페이스+이름 한 쌍) 같은 기본 식별자 타입이다.
	"k8s.io/apimachinery/pkg/types"
	// intstr: "정수 또는 문자열" 둘 다 될 수 있는 필드용 타입이다(포트 이름/번호에 쓴다).
	"k8s.io/apimachinery/pkg/util/intstr"
	// ptr: 값으로부터 포인터를 만들어 주는 제네릭 헬퍼다(ptr.To).
	// 쿠버네티스 API에는 *int32 같은 포인터 필드가 많은데, Go는 리터럴의 주소를 바로 못 얻어서 이런 헬퍼가 필요하다.
	"k8s.io/utils/ptr"
	// ctrl: controller-runtime의 최상위 패키지이며, Request/Result/Manager/빌더가 여기 있다.
	ctrl "sigs.k8s.io/controller-runtime"
	// client: 쿠버네티스 객체를 읽고 쓰는 클라이언트 인터페이스와 헬퍼(IgnoreNotFound 등)다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// controllerutil: CreateOrUpdate, SetControllerReference 같은 controller 작성용 유틸이다.
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	// logf: context에 실려 오는 구조화 로거를 꺼내 쓰는 패키지다(logf.FromContext).
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(InferenceDeployment 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// nvidiaGPUResource: NVIDIA GPU를 요청할 때 쓰는 node 확장 resource 이름이다.
//
// Go 문법 설명:
//   - const 는 컴파일 시점에 값이 정해지는 상수 선언 키워드다.
//   - corev1.ResourceName("nvidia.com/gpu")는 문자열을 ResourceName이라는 전용 타입으로 변환하는 표현이다.
//     Go는 문자열 기반 타입이라도 명시적 변환을 요구하므로 이런 형태가 필요하다.
//   - 소문자로 시작하므로 이 패키지 안에서만 보이는 비공개 상수다.
//
// 서빙 pod가 GPU 하나당 요청하는 node 확장 resource로,
// pod 수준 resource이며 GPUQuotaPolicy의 ResourceQuota 키와는 별개다.
const nvidiaGPUResource = corev1.ResourceName("nvidia.com/gpu")

// const 블록: 관련 상수를 괄호로 묶어 한 번에 선언한다.
const (
	// instanceLabel: 어느 InferenceDeployment의 pod인지 표시하는 표준 label 키다.
	//
	// 하나의 InferenceDeployment가 소유한 pod를 선택하는 label로,
	// Deployment의 selector는 생성 후 불변이라 한 번 설정하고 이후 변경하지 않는다.
	instanceLabel = "app.kubernetes.io/instance"
)

// InferenceDeploymentReconciler: InferenceDeployment 객체를 조정(reconcile)하는 주체다.
//
// Go 문법 설명:
//   - type 이름 struct { ... } 는 여러 필드를 묶는 사용자 정의 타입(구조체) 선언이다.
//   - client.Client 처럼 필드 이름 없이 타입만 적으면 "임베딩(embedding)"이다.
//     임베딩하면 그 타입의 메서드가 이 구조체의 메서드처럼 승격되어, r.Get(...) / r.Status() 를 바로 쓸 수 있다.
//   - Scheme *runtime.Scheme 는 이름 있는 일반 필드이며, 별표(*)는 포인터를 뜻한다.
//     Scheme는 Go 타입과 API 그룹/버전을 잇는 매핑표이고, owner 참조를 만들 때 필요하다.
//
// InferenceDeployment 객체를 조정하는 reconciler
type InferenceDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// 아래 네 줄은 주석처럼 보이지만 실제로는 kubebuilder가 읽는 "마커(marker)"다.
// make manifests 를 돌리면 이 마커들로부터 config/rbac 의 ClusterRole YAML이 생성된다.
// 즉 이 controller가 어떤 API에 어떤 동작(verb)을 할 수 있는지 선언하는 코드이므로 번역하거나 고치면 안 된다.
// InferenceDeployment 본체는 읽기만(get/list/watch) 하고 status만 갱신하며,
// Deployment와 Service는 생성/수정까지 하지만 delete 권한은 없다(정리는 owner 참조를 통한 GC가 담당).

// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=inferencedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch

// Reconcile: InferenceDeployment 하나를 원하는 상태로 맞추는 핵심 함수다.
//
// Go 문법 설명:
//   - func (r *InferenceDeploymentReconciler) 부분은 리시버(receiver)다.
//     이 함수가 InferenceDeploymentReconciler 타입에 붙는 "메서드"라는 뜻이다.
//   - *InferenceDeploymentReconciler 처럼 별표를 붙이면 값 복사가 아니라 원본 포인터를 받는다.
//   - ctrl.Request는 "어떤 객체를 조정하라"는 지시이며, 실제 객체가 아니라 이름/네임스페이스만 담고 있다.
//     객체 내용은 controller가 직접 캐시에서 Get으로 읽어야 하는데, 그래야 항상 최신 상태를 보게 된다.
//   - ctrl.Result는 "다음에 언제 다시 부를지"를 알려주는 반환값이다.
//     ctrl.Result{} 처럼 빈 값이면 "즉시 재큐잉할 필요 없음"이라는 뜻이다.
//   - 반환값이 (ctrl.Result, error) 두 개인데, error가 nil이 아니면 controller-runtime이 지수 백오프로 자동 재시도한다.
//
// reconcile은 몇 번을 실행해도 결과가 같아야 한다(멱등성).
// 같은 이벤트가 중복 전달되거나 재시도가 일어나도 부작용이 없어야 하기 때문이다.
//
// InferenceDeployment로부터 Deployment와 Service를 동기화하고 준비 상태를 status에 반영한다.
//
// 재조정 흐름:
//  1. 같은 이름의 Deployment가 남의 소유면 덮어쓰지 않고 Degraded로 보고한다,
//  2. CreateOrUpdate로 소유 Deployment를 원하는 spec(replica·image·GPU·probe)에 맞추고 owner 참조를 걸어 GC와 연동한다,
//  3. Service도 같은 방식으로 소유권을 확인한 뒤 ClusterIP Service를 동기화한다,
//  4. Deployment status로부터 phase(Pending/Progressing/Ready/Degraded)와 Available condition을 도출한다,
//  5. status가 실제로 바뀐 경우에만 갱신한다.
func (r *InferenceDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// logf.FromContext(ctx)는 controller-runtime이 ctx에 심어 둔 로거를 꺼낸다.
	// 이 로거에는 이미 controller 이름과 대상 객체 정보가 붙어 있어 로그 추적이 쉬워진다.
	log := logf.FromContext(ctx)

	// var infd ... : 읽어 온 InferenceDeployment를 담을 빈 변수를 선언한다.
	// 포인터가 아닌 값으로 선언한 뒤 아래에서 &infd로 주소를 넘기는 흔한 패턴이다.
	var infd platformv1.InferenceDeployment
	// r.Get(...)은 임베딩된 client.Client의 메서드이며, 캐시에서 객체를 읽어 infd에 채운다.
	// req.NamespacedName은 이 요청이 가리키는 객체의 네임스페이스+이름이다.
	// &infd 의 & 는 주소(포인터)를 넘긴다는 뜻이며, Get이 infd 원본을 채워야 하므로 포인터가 필요하다.
	if err := r.Get(ctx, req.NamespacedName, &infd); err != nil {
		// client.IgnoreNotFound(err)는 "없음(NotFound)" 에러면 nil을, 그 외 에러면 err을 그대로 돌려준다.
		// 객체가 이미 삭제된 뒤 큐에 남아 있던 이벤트가 도착하는 것은 정상 상황이다.
		// 이때 에러를 반환하면 사라진 객체를 영원히 재시도하게 되므로 조용히 종료한다.
		return ctrl.Result{}, client.IgnoreNotFound(err) // 이미 삭제된 경우는 오류로 다루지 않음
	}

	// 소유권 방어: 실제 객체를 만들기 전에 "이 이름을 이미 남이 쓰고 있는지"부터 확인한다.
	//
	// 설계 근거(설계서 Components 절): 아래 CreateOrUpdate는 이름이 같은 객체가 있으면 그것을 그대로 수정한다.
	// 즉 확인 없이 부르면 다른 팀이 운영 중인 Deployment를 우리 spec으로 덮어써 버린다.
	// 이는 InferenceDeployment 하나만 만들면 남의 워크로드를 탈취할 수 있다는 뜻이므로 멀티테넌트 환경에서 치명적이다.
	// 그래서 "소유하지 않은 동명 객체는 절대 입양(adopt)하지 않고" Degraded로 보고만 하고 물러난다.
	//
	// Go 문법 설명:
	//   - if a, b := f(); 조건 { ... } else if 조건 { ... } 은 호출 결과를 지역 변수로 받아 바로 검사하는 관용구다.
	//     여기서 만든 conflict/err는 이 if-else 체인 안에서만 유효하다.
	//   - &appsv1.Deployment{} 는 빈 Deployment를 만들고 그 주소를 넘기는 표현이며, 결과를 담을 그릇 역할이다.
	//     이 그릇의 타입이 ownedConflict에게 "무슨 종류의 객체를 조회할지" 알려 준다.
	//
	// 같은 이름의 Deployment가 남의 소유면 덮어쓰지 않음
	if conflict, err := r.ownedConflict(ctx, &infd, &appsv1.Deployment{}); err != nil {
		// fmt.Errorf의 %w 동사는 원본 에러를 "감싸(wrap)" 보존한다.
		// 그래서 호출한 쪽에서 errors.Is/errors.As로 원인을 그대로 판별할 수 있다.
		return ctrl.Result{}, fmt.Errorf("check deployment ownership %s/%s: %w", infd.Namespace, infd.Name, err)
	} else if conflict {
		// 충돌은 재시도해도 저절로 풀리지 않는 결정적 실패다.
		// 그래서 에러를 반환해 백오프 재시도를 유발하지 않고 status에 Degraded로 남겨 운영자가 조치하게 한다.
		log.Info("Deployment exists and is not owned by this InferenceDeployment; refusing to adopt", "name", infd.Name)
		return r.markDegraded(ctx, &infd, infdReasonConflict, "a Deployment of the same name is not owned by this InferenceDeployment")
	}

	// dep: 우리가 원하는 Deployment를 가리킬 그릇이며, 지금은 이름/네임스페이스만 채워 둔다.
	// CreateOrUpdate가 이 이름으로 실물을 찾아 나머지를 채워 준다.
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: infd.Name, Namespace: infd.Namespace}}
	// controllerutil.CreateOrUpdate: 없으면 만들고 있으면 고치는 표준 헬퍼다.
	// 동작 순서는 (1)dep 이름으로 실물을 Get, (2)마지막 인자인 mutate 함수를 호출해 dep를 원하는 모습으로 고침, (3)Create 또는 Update다.
	//
	// Go 문법 설명:
	//   - func() error { ... } 는 이름 없는 함수(클로저)를 그 자리에서 만들어 인자로 넘기는 것이다.
	//     이 클로저는 바깥의 infd와 dep 변수를 그대로 붙잡아 쓸 수 있다.
	//   - if _, err := ...; err != nil 의 밑줄(_)은 "이 반환값은 안 쓰겠다"는 표시다.
	//     첫 반환값은 결과 종류(created/updated/unchanged)인데 여기서는 필요 없다.
	//
	// 중요한 점은 mutate 함수가 실물 위에서 실행된다는 것이다.
	// 그래서 우리가 건드리지 않은 필드(예: clusterIP, 다른 controller가 붙인 annotation)는 보존된다.
	// CreateOrUpdate는 mutate 전후를 비교해 실제로 달라진 게 없으면 Update를 아예 보내지 않으므로 멱등성도 확보된다.
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		// 우리가 관리하는 필드만 원하는 값으로 덮어쓴다.
		r.mutateDeployment(&infd, dep)
		// SetControllerReference: dep의 ownerReferences에 "infd가 이 객체의 controller"라고 기록한다.
		// 이 한 줄이 두 가지를 동시에 해결한다.
		// 첫째, infd가 삭제되면 쿠버네티스 GC가 dep를 자동으로 같이 지운다(그래서 delete RBAC이 필요 없다).
		// 둘째, 아래 metav1.IsControlledBy와 Owns()가 이 표시를 근거로 소유권을 판단한다.
		// Scheme이 필요한 이유는 owner의 apiVersion/kind를 Go 타입으로부터 알아내야 하기 때문이다.
		return controllerutil.SetControllerReference(&infd, dep, r.Scheme) // owner 참조 설정으로 소유권 표시 및 GC 연동
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync deployment %s/%s: %w", infd.Namespace, infd.Name, err)
	}

	// Service에도 같은 방어를 적용한다.
	// Service 탈취는 특히 위험한데, selector를 우리 것으로 바꾸는 순간 남의 트래픽이 우리 pod로 흘러들기 때문이다.
	//
	// Service도 마찬가지로 소유권 충돌 확인
	if conflict, err := r.ownedConflict(ctx, &infd, &corev1.Service{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("check service ownership %s/%s: %w", infd.Namespace, infd.Name, err)
	} else if conflict {
		log.Info("Service exists and is not owned by this InferenceDeployment; refusing to adopt", "name", infd.Name)
		return r.markDegraded(ctx, &infd, infdReasonServiceConflict, "a Service of the same name is not owned by this InferenceDeployment")
	}

	// Deployment와 완전히 같은 패턴으로 Service를 동기화한다.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: infd.Name, Namespace: infd.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		r.mutateService(&infd, svc)
		return controllerutil.SetControllerReference(&infd, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync service %s/%s: %w", infd.Namespace, infd.Name, err)
	}

	// req.String()은 "namespace/name" 형태의 문자열을 만들어 준다.
	log.Info("Synced serving objects", "inferenceDeployment", req.String())

	// 여기서부터는 status 계산 구간이다.
	// 위에서 spec을 맞췄으니 이제 실제 상태(dep.Status)를 읽어 사용자에게 보여 줄 요약을 만든다.
	//
	// Go 문법 설명: Go는 값을 여러 개 돌려줄 수 있어서 phase와 cond를 한 번에 받는다.
	phase, cond := computeInfDPhase(&infd, dep)
	// DeepCopy(): 현재 status를 통째로 복제한다.
	// 원본을 직접 고치지 않고 복제본에 원하는 값을 채운 뒤 마지막에 비교하기 위해서다.
	// 이렇게 해야 "원본 vs 원하는 값"을 비교해 정말 바뀐 경우에만 API를 호출할 수 있다.
	desired := infd.Status.DeepCopy()
	desired.Phase = phase
	desired.ReadyReplicas = dep.Status.ReadyReplicas
	// ObservedGeneration: "이 status는 spec의 몇 번째 generation을 보고 만든 것인가"를 기록한다.
	// Generation은 spec이 바뀔 때마다 API 서버가 1씩 올리는 번호다.
	// 사용자가 이 둘을 비교하면 지금 보는 status가 최신 spec 기준인지(stale이 아닌지) 알 수 있다.
	desired.ObservedGeneration = infd.Generation
	// meta.SetStatusCondition: 같은 Type의 condition이 있으면 교체하고 없으면 추가하는 표준 헬퍼다.
	// 직접 슬라이스를 다루지 않고 이걸 쓰는 이유는 LastTransitionTime을 올바르게 관리해 주기 때문이다.
	// 구체적으로는 Status(True/False)가 실제로 바뀐 경우에만 시각을 갱신하고, Reason만 바뀌면 유지한다.
	// &desired.Conditions 처럼 주소를 넘기는 이유는 이 함수가 슬라이스 원본을 수정해야 하기 때문이다.
	meta.SetStatusCondition(&desired.Conditions, cond)

	// equality.Semantic.DeepEqual: 두 status를 의미적으로 비교한다.
	// 일반 reflect.DeepEqual과 달리 nil 슬라이스와 빈 슬라이스를 같다고 보는 등 쿠버네티스 관례를 안다.
	// ! 는 부정 연산자이므로 이 조건은 "다를 때만"이라는 뜻이다.
	//
	// 설계 근거(설계서 Components 절): 바뀐 게 없는데 Update를 보내면 resourceVersion이 올라간다.
	// 그러면 watch 이벤트가 발생해 이 controller가 자기 자신을 다시 깨우는 무한 reconcile 루프가 된다.
	// 이 가드 한 줄이 그 루프를 끊고, 테스트의 "is idempotent once steady"가 검증하는 성질을 보장한다.
	if !equality.Semantic.DeepEqual(infd.Status, *desired) { // 실제 바뀐 게 있을 때만 status 갱신
		// *desired 의 별표는 "포인터가 가리키는 값 자체"를 꺼내는 역참조다.
		infd.Status = *desired
		// r.Status().Update(...)는 status 서브리소스만 갱신한다.
		// spec을 건드리는 일반 Update와 분리되어 있어서, 사용자가 동시에 spec을 바꿔도 서로 덮어쓰지 않는다.
		if err := r.Status().Update(ctx, &infd); err != nil {
			return ctrl.Result{}, fmt.Errorf("update inferencedeployment status %s/%s: %w", infd.Namespace, infd.Name, err)
		}
		log.Info("Updated InferenceDeployment status", "name", infd.Name, "phase", phase)
	}
	// 정상 종료이며, 빈 Result와 nil 에러는 "할 일 다 했고 재큐잉 불필요"를 뜻한다.
	// 이후 변화는 watch 이벤트가 알아서 이 함수를 다시 부른다.
	return ctrl.Result{}, nil
}

// servingPort: 이 InferenceDeployment가 쓸 서빙 port 번호를 정한다.
//
// Go 문법 설명:
//   - 리시버가 없는 일반 함수다(특정 타입에 붙지 않음).
//   - int32는 32비트 정수이며, 쿠버네티스 API의 port 필드가 이 타입이라 맞춰 쓴다.
//   - Go에는 기본 인자 값 문법이 없어서, "0이면 기본값"이라는 규칙을 이렇게 함수로 직접 표현한다.
//
// 설정된 서빙 port 반환, 미설정 시 8080 기본값
func servingPort(infd *platformv1.InferenceDeployment) int32 {
	// Go의 숫자 필드는 값을 안 넣으면 자동으로 0(제로 값)이 된다.
	// 그래서 0은 "미설정"과 사실상 같은 의미이며, 포트 0은 어차피 유효한 서빙 포트가 아니다.
	if infd.Spec.Port == 0 {
		return 8080
	}
	return infd.Spec.Port
}

// infdLabels: 우리가 만드는 Deployment/Service에 공통으로 붙일 label 묶음을 만든다.
//
// Go 문법 설명:
//   - 반환 타입 map[string]string 은 "문자열 키로 문자열 값을 찾는 해시맵"이다.
//   - map[string]string{...} 처럼 중괄호 안에 키:값을 적으면 만들면서 바로 채울 수 있다(맵 리터럴).
//   - 키 자리에 instanceLabel 처럼 상수 이름을 쓸 수도 있고, 이때는 따옴표 없이 적는다.
//   - 마지막 항목 뒤의 쉼표는 Go에서 필수다.
//
// 설계 근거(설계서 Components 절): app.kubernetes.io/* 는 쿠버네티스 권장 공통 label이다.
// managed-by는 "이건 사람이 손대는 게 아니라 control plane이 관리한다"를 명시해 오조작을 줄인다.
// tenant label에 Namespace를 넣는 이유는 이 플랫폼이 네임스페이스를 테넌트 경계로 쓰기 때문이다.
//
// 소유한 Deployment와 Service에 붙이는 권장 label set
func infdLabels(infd *platformv1.InferenceDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":              "inferencedeployment",
		instanceLabel:                         infd.Name,
		"app.kubernetes.io/managed-by":        "gpu-platform-control-plane",
		"platform.lkhun9311.github.io/tenant": infd.Namespace,
	}
}

// mutateDeployment: Deployment를 우리가 원하는 모습으로 고친다.
// 위 CreateOrUpdate의 클로저 안에서 호출되며, 인자로 받은 dep는 이미 클러스터에서 읽어 온 실물이다.
//
// Go 문법 설명:
//   - 반환값이 없는 메서드이며, dep가 포인터이므로 이 함수가 고친 내용이 호출한 쪽에 그대로 보인다.
//     (포인터가 아니라 값으로 받았다면 복사본만 고치고 끝나서 아무 효과가 없다.)
//
// 설계 근거(설계서 Components 절): 이 함수는 "우리가 관리한다고 선언한 필드"만 건드린다.
// dep 전체를 새로 만들어 대입하지 않는 이유는 그러면 실물에 있던 다른 필드가 다 날아가기 때문이다.
// 이 선택적 덮어쓰기가 drift 복구("restores a drifted Deployment image" 테스트)를 가능하게 한다.
// 즉 누가 image를 바꿔 놔도 다음 reconcile에서 우리 값으로 되돌아온다.
//
// 이 controller가 관리하는 field만 Deployment에 설정하고,
// selector는 생성 후 불변이라 한 번만 설정하고 이후 건드리지 않는다.
func (r *InferenceDeploymentReconciler) mutateDeployment(infd *platformv1.InferenceDeployment, dep *appsv1.Deployment) {
	labels := infdLabels(infd)
	port := servingPort(infd)

	dep.Labels = labels
	// ptr.To(...)는 값을 받아 그 값을 담은 새 포인터를 돌려주는 제네릭 헬퍼다.
	// Replicas 필드가 *int32(포인터)인 이유는 "0으로 설정함"과 "설정 안 함"을 구분해야 하기 때문이다.
	// 포인터가 아니면 둘 다 0이 되어 구분할 수 없다.
	dep.Spec.Replicas = ptr.To(infd.Spec.Replicas)
	// ProgressDeadlineSeconds: 이 시간 안에 rollout이 진전되지 않으면 Deployment가 스스로 실패로 표시한다.
	// int32(600)은 리터럴 600을 int32 타입으로 변환한 것이며, ptr.To에 정확한 타입을 주기 위해 필요하다.
	// 이 값을 명시해야 아래 computeInfDPhase의 Degraded 판정(ProgressDeadlineExceeded)이 동작한다.
	//
	// 이어지는 if문: selector는 Deployment가 생성된 뒤에는 API 서버가 변경을 거부하는 불변(immutable) 필드다.
	// 그래서 nil일 때(= 아직 실물이 없는 최초 생성 때)만 채우고, 이미 값이 있으면 절대 건드리지 않는다.
	// 만약 매번 덮어쓰면 기존 Deployment 갱신이 API 에러로 영원히 실패하게 된다.
	dep.Spec.ProgressDeadlineSeconds = ptr.To(int32(600)) // rollout 실패 판정 시한 600초
	if dep.Spec.Selector == nil {                         // 최초 생성 때만 selector 지정 (불변 field)
		// selector에는 label 전체가 아니라 instanceLabel 하나만 쓴다.
		// selector에 넣은 label은 사실상 불변이 되므로, 나중에 바뀔 수 있는 label(managed-by 등)은 넣지 않는 게 안전하다.
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: infd.Name}}
	}
	// pod template의 label은 selector와 반드시 맞아야 한다.
	// selector가 instanceLabel만 보므로, label을 더 붙이는 것은 문제가 없다.
	dep.Spec.Template.Labels = labels

	// container: 서빙 컨테이너 하나를 통째로 새로 구성한다.
	//
	// Go 문법 설명:
	//   - corev1.Container{...} 는 구조체 리터럴이며, 필드이름: 값 형태로 원하는 필드만 채운다.
	//     적지 않은 필드는 자동으로 제로 값(0, "", nil)이 된다.
	//   - []string{...} 은 문자열 슬라이스 리터럴이다.
	//   - []corev1.ContainerPort{{...}} 처럼 안쪽 타입 이름을 생략할 수 있는데, 슬라이스 원소 타입이 이미 정해져 있어서다.
	container := corev1.Container{
		Name:  "server",
		Image: infd.Spec.Image,
		Args:  []string{"--model", infd.Spec.Model.Name, "--model-path", infd.Spec.Model.StorageURI},
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
		// ReadinessProbe: 이 pod가 트래픽을 받을 준비가 됐는지 검사한다.
		// 실패하면 pod는 죽지 않고 Service 엔드포인트에서만 빠지며, 이게 아래 ReadyReplicas 계산의 근거가 된다.
		// intstr.FromString("http")는 포트 번호 대신 위에서 붙인 포트 "이름"으로 가리키는 것이다.
		// 이름으로 참조하면 나중에 포트 번호가 바뀌어도 probe를 같이 고칠 필요가 없다.
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		}},
		// LivenessProbe: 컨테이너가 살아 있는지 검사하며, 실패하면 kubelet이 컨테이너를 재시작한다.
		// readiness와 달리 이쪽은 실패가 곧 재시작이므로 성격이 다르다.
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		}},
	}
	// GPUCount가 0이면 Resources를 아예 설정하지 않는다.
	// 확장 resource는 0을 요청하는 것과 요청하지 않는 것이 다르게 취급될 수 있어서, 그냥 비워 두는 게 옳다.
	// container를 매번 새로 만들기 때문에, GPUCount가 1에서 0으로 바뀌면 이 필드가 자연히 사라진다.
	// (테스트 "removes the GPU resource when GPUCount changes from 1 to 0"이 이 동작을 검증한다.)
	if infd.Spec.GPUCount > 0 { // GPU를 요청한 경우에만 requests/limits 지정
		// resource.NewQuantity(값, 형식)은 자원 수량 객체의 포인터를 돌려준다.
		// 앞의 별표(*)는 그 포인터를 역참조해 값 자체를 꺼내는 것이며, ResourceList가 포인터가 아닌 값을 담기 때문이다.
		// int64(...)는 int32를 int64로 넓히는 변환이고, DecimalSI는 "1", "2" 같은 십진 표기를 뜻한다.
		q := *resource.NewQuantity(int64(infd.Spec.GPUCount), resource.DecimalSI)
		// GPU 같은 확장 resource는 쿠버네티스가 requests와 limits를 반드시 같게 요구한다.
		// GPU는 CPU처럼 잘게 나눠 쓸 수 없어 오버커밋 자체가 불가능하기 때문이다.
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{nvidiaGPUResource: q},
			Limits:   corev1.ResourceList{nvidiaGPUResource: q}, // GPU는 requests와 limits를 같게 둠
		}
	}
	// Containers 슬라이스를 통째로 교체한다.
	// 이렇게 해야 drift(누가 컨테이너를 추가하거나 image를 바꾼 경우)가 확실히 되돌려진다.
	dep.Spec.Template.Spec.Containers = []corev1.Container{container}
}

// mutateService: Service를 우리가 원하는 모습으로 고친다.
// mutateDeployment와 같은 원칙으로, 우리가 관리하는 필드만 덮어쓴다.
//
// 설계 근거(설계서 Components 절): 여기서 spec 전체를 교체하지 않는 것이 중요하다.
// Service의 clusterIP는 API 서버가 할당하는 불변 필드라서, 지웠다가 다시 쓰면 갱신이 거부된다.
// 우리가 안 건드리면 CreateOrUpdate가 읽어 온 실물의 값이 그대로 보존된다.
//
// 이 controller가 관리하는 field만 Service에 설정
func (r *InferenceDeploymentReconciler) mutateService(infd *platformv1.InferenceDeployment, svc *corev1.Service) {
	port := servingPort(infd)
	svc.Labels = infdLabels(infd)
	// ClusterIP: 클러스터 내부에서만 접근 가능한 가상 IP를 받는 기본 Service 타입이다.
	// 외부 노출은 게이트웨이가 담당하므로 서빙 Service를 직접 외부에 열지 않는다.
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	// Service의 selector는 Deployment의 selector와 같은 label을 써야 우리 pod를 찾는다.
	svc.Spec.Selector = map[string]string{instanceLabel: infd.Name}
	// Port는 Service가 노출하는 포트이고, TargetPort는 실제로 트래픽을 보낼 pod 쪽 포트다.
	// TargetPort를 번호가 아니라 이름("http")으로 참조해 위에서 붙인 컨테이너 포트를 가리킨다.
	// 이름으로 참조하면 나중에 포트 번호가 바뀌어도 이쪽을 같이 고칠 필요가 없다.
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       "http",
		Port:       port,
		TargetPort: intstr.FromString("http"),
	}}
}

// phase 값과 condition의 Reason 문자열을 상수로 모아 둔다.
// 문자열을 코드 여기저기에 직접 적으면 오타가 나도 컴파일러가 못 잡지만, 상수로 두면 잡아 준다.
//
// 첫 그룹은 status.phase에 들어갈 수 있는 값의 전부다.
//   - Pending: 아직 준비된 replica가 하나도 없다.
//   - Progressing: 변화가 진행 중이며 아직 목표에 수렴하지 않았다.
//   - Ready: 원하는 상태에 완전히 도달했다.
//   - Degraded: 저절로 회복되지 않는 문제가 있어 사람의 조치가 필요하다.
//
// 둘째 그룹은 condition의 Type과 Reason 값들이다.
// phase는 사람이 한눈에 보는 요약이고, condition은 기계가 읽는 상세 사유라는 역할 분담이다.
// Reason은 왜 그 상태인지를 기계 판독 가능하게 알려 주며, 쿠버네티스 관례상 공백 없는 CamelCase여야 한다.
//   - ScaledToZero: 의도적으로 0 replica이며 이는 정상이다.
//   - RolloutInProgress: rollout이 아직 진행 중이다.
//   - MinimumReplicasAvailable: 필요한 replica가 모두 준비됐다.
//   - DeploymentConflict: 동명의 Deployment를 남이 소유하고 있다.
//   - ServiceConflict: 동명의 Service를 남이 소유하고 있다.
const (
	infdPhasePending     = "Pending"
	infdPhaseProgressing = "Progressing"
	infdPhaseReady       = "Ready"
	infdPhaseDegraded    = "Degraded"

	infdCondAvailable         = "Available"
	infdReasonScaledZero      = "ScaledToZero"
	infdReasonRollout         = "RolloutInProgress"
	infdReasonAvailable       = "MinimumReplicasAvailable"
	infdReasonConflict        = "DeploymentConflict"
	infdReasonServiceConflict = "ServiceConflict"
)

// computeInfDPhase: Deployment의 실제 status를 보고 InferenceDeployment의 phase와 condition을 계산한다.
//
// Go 문법 설명:
//   - 리시버가 없는 순수 함수다.
//     클러스터를 읽거나 쓰지 않고 입력만 보고 답을 내므로 테스트하기 쉽고 부작용이 없다.
//   - 반환 타입 (string, metav1.Condition)은 phase 문자열과 condition을 한 번에 돌려준다는 뜻이다.
//
// 왜 순서가 중요한가:
// 아래 판정은 위에서부터 차례로 검사하며 처음 걸리는 곳에서 즉시 return한다.
// 즉 이건 단순한 조건 나열이 아니라 우선순위 사다리이고, 두 규칙을 맞바꾸면 결과가 달라진다.
// 특히 1번(stale gate)과 2번(ScaledToZero)의 순서가 뒤바뀌면 scale-down 시 잘못된 Ready 보고가 나온다.
// 자세한 이유는 각 단계의 주석에 적었다.
//
// Deployment status로부터 phase와 Available condition 도출
// 우선순위 (위에서 아래로)
//  1. stale gate, spec.Replicas > 0 이고 ObservedGeneration < Generation 이면 Progressing으로 두어,
//     Deployment가 아직 관측하지 못한 0으로의 scale-down이 성급하게 Ready로 보고되지 않도록 한다.
//  2. ScaledToZero, Replicas == 0 이면 Ready (0 replica는 spec만으로 항상 확정)
//  3. Degraded, Deployment의 Progressing condition이 ProgressDeadlineExceeded로 False
//  4. Pending, ReadyReplicas == 0
//  5. Progressing, UpdatedReplicas나 ReadyReplicas가 spec.Replicas 미만이거나 옛 replica가 아직 안 빠짐
//  6. Ready, 완전히 수렴
func computeInfDPhase(infd *platformv1.InferenceDeployment, dep *appsv1.Deployment) (string, metav1.Condition) {
	// avail: Available condition을 만드는 작은 헬퍼를 지역 변수에 담았다.
	//
	// Go 문법 설명:
	//   - 이름 없는 함수(클로저)를 변수에 대입하면 그 변수를 함수처럼 호출할 수 있다.
	//   - 이 클로저는 바깥의 infd를 붙잡아 쓰므로 ObservedGeneration을 매번 인자로 넘길 필요가 없다.
	//   - (reason, msg string) 처럼 같은 타입 인자는 타입을 한 번만 적어도 된다.
	//
	// 매번 condition을 손으로 만들면 ObservedGeneration을 빠뜨리기 쉬워서 한 곳으로 모았다.
	// condition의 ObservedGeneration은 "이 판정이 어느 spec generation 기준인지"를 남겨,
	// 사용자가 오래된 condition을 최신인 것으로 오해하지 않게 한다.
	avail := func(status metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: infdCondAvailable, Status: status, Reason: reason, Message: msg, ObservedGeneration: infd.Generation}
	}
	// 1. stale gate, Deployment controller가 현재 spec을 관측할 때까지 phase 판단을 보류하고,
	// spec이 0 replica를 원하고 status도 이미 0개를 보이는 경우에만 gate를 생략하는데,
	// 빠질 replica가 없고 spec 의도가 확정적이기 때문이다.
	//
	// 이 gate가 왜 맨 위에 있어야 하는가:
	// 우리가 방금 dep.Spec.Replicas를 바꿔 써도, dep.Status는 아직 Deployment controller가 갱신하기 전이라 옛날 값이다.
	// 이 낡은 status를 최신인 양 읽고 판정하면 틀린 답이 나온다.
	// 예를 들어 replica 2에서 0으로 줄이는 순간을 보자.
	// 아래 2번(ScaledToZero)이 먼저 걸리면 spec.Replicas == 0이라는 이유만으로 즉시 Ready를 보고한다.
	// 하지만 현실에서는 pod 2개가 아직 멀쩡히 떠 있고 종료조차 시작되지 않았다.
	// 즉 "다 내려갔다"고 거짓 보고를 하는 것이며, 이걸 믿고 다음 단계를 진행하는 자동화가 있으면 사고가 난다.
	// 그래서 stale gate가 2번보다 반드시 위에 있어야 한다.
	//
	// staleDep: Deployment의 status가 아직 최신 spec을 반영하지 못했는지 여부다.
	// Generation은 spec이 바뀔 때마다 올라가고, Status.ObservedGeneration은 controller가 따라잡을 때 갱신된다.
	// 그래서 ObservedGeneration < Generation 이면 "아직 못 따라잡음(stale)"이다.
	staleDep := dep.Status.ObservedGeneration < dep.Generation
	// zeroAndDrained: gate를 건너뛰어도 안전한 유일한 예외다.
	// spec이 0을 원하고(의도가 확정적) status의 실제 replica도 이미 0이면(더 빠질 게 없음),
	// status가 stale이든 아니든 결론은 어차피 "0개로 도달함"이라 바뀔 여지가 없다.
	// 이 예외가 없으면 0 replica로 만든 객체가 stale gate에 영원히 걸려 Ready에 도달하지 못한다.
	zeroAndDrained := infd.Spec.Replicas == 0 && dep.Status.Replicas == 0
	// && 는 "그리고(AND)", ! 는 부정 연산자다.
	// 즉 "stale인데 위 예외에도 해당하지 않으면" 판정을 보류하고 Progressing으로 둔다.
	if staleDep && !zeroAndDrained {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "deployment not yet observed")
	}
	// 2. ScaledToZero, 의도적인 0 replica는 status가 실행 중 replica 0을 보이면 Ready
	//
	// 여기 도달했다는 것은 stale gate를 통과했다는 뜻이다.
	// 즉 status가 최신이거나, 최신이 아니어도 이미 0개로 비워진 상태다.
	// 두 경우 모두 "0 replica가 실제로 달성됨"이 보장되므로 Ready로 올려도 거짓말이 아니다.
	// 0 replica는 실패가 아니라 사용자가 의도한 정상 상태라서 Degraded가 아닌 Ready로 본다.
	if infd.Spec.Replicas == 0 {
		return infdPhaseReady, avail(metav1.ConditionTrue, infdReasonScaledZero, "scaled to zero replicas")
	}
	// 3. Degraded, ProgressDeadlineExceeded condition은 rollout이 스스로 완료되지 못함을 의미
	//
	// 이 검사가 4번(Pending)보다 위에 있는 이유는, 실패한 rollout도 ReadyReplicas가 0이기 때문이다.
	// 순서가 반대면 영원히 실패한 rollout을 계속 "Pending(곧 될 거예요)"으로 보고하게 되어,
	// 운영자가 문제를 알아채지 못한다.
	//
	// Go 문법 설명:
	//   - for i := range ... 는 슬라이스를 처음부터 끝까지 훑는 반복문이며, range는 인덱스를 0,1,2...로 준다.
	//   - dep.Status.Conditions는 map이 아니라 슬라이스라서 Type으로 바로 찾을 수 없고 이렇게 순회해야 한다.
	for i := range dep.Status.Conditions {
		c := dep.Status.Conditions[i]
		// 세 조건이 모두 맞아야 진짜 실패다.
		// Progressing 타입이면서, 상태가 False이고, 사유가 ProgressDeadlineExceeded여야 한다.
		// Progressing=False라도 사유가 다르면 실패가 아닐 수 있어서 Reason까지 확인한다.
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return infdPhaseDegraded, avail(metav1.ConditionFalse, "ProgressDeadlineExceeded", "deployment rollout failed")
		}
	}
	// 4. Pending, 아직 준비된 replica 없음
	//
	// replica를 1개 이상 원하는데 준비된 게 0개면 아직 트래픽을 전혀 처리할 수 없는 상태다.
	// 이걸 Progressing과 구분하는 이유는 "하나도 안 됨"과 "일부만 됨"의 운영상 의미가 다르기 때문이다.
	if dep.Status.ReadyReplicas == 0 {
		return infdPhasePending, avail(metav1.ConditionFalse, infdReasonRollout, "no replicas ready yet")
	}
	// 5. Progressing, replica가 아직 완전히 갱신/준비되지 않았거나 옛 replica가 안 빠짐
	//
	// UpdatedReplicas는 최신 pod template으로 만들어진 replica 수이고, ReadyReplicas는 준비된 replica 수다.
	// 둘 중 하나라도 원하는 수에 못 미치면 아직 수렴하지 않았다.
	// || 는 "또는(OR)"이며, 앞 조건이 참이면 뒤는 검사하지 않는다(short-circuit).
	if dep.Status.UpdatedReplicas < infd.Spec.Replicas || dep.Status.ReadyReplicas < infd.Spec.Replicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "rollout in progress")
	}
	// Replicas(전체)와 UpdatedReplicas(최신 것)가 다르면 옛 template의 pod가 아직 남아 있다는 뜻이다.
	// 새 pod는 다 떴지만 옛 pod가 아직 안 죽은 rolling update 중간 상태가 여기 해당한다.
	if dep.Status.Replicas != dep.Status.UpdatedReplicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "waiting for old replicas to drain")
	}
	// 6. Ready, scale-down 도중 남은 잉여 replica가 아직 제거되지 않은 상황을 막기 위해,
	// 세 replica count가 모두 원하는 값과 같아야 한다.
	//
	// 왜 이 검사가 따로 필요한가:
	// 위 5번은 전부 "미만(<)" 비교라서 개수가 남아도는 경우를 못 잡는다.
	// replica를 3에서 2로 줄이는 중이라면 세 count가 모두 3일 수 있는데,
	// 3 >= 2 이므로 5번을 그냥 통과해 버린다.
	// 하지만 pod가 3개 떠 있는 상태를 "원하는 2개에 도달함(Ready)"이라고 하면 거짓이다.
	// 그래서 여기서는 "이상/이하"가 아니라 정확히 같은지(==)를 세 count 모두에 대해 확인한다.
	// (테스트 "reports Progressing (not Ready) when Replicas=3 but desired is 2"가 이 회귀를 막는다.)
	if dep.Status.Replicas != infd.Spec.Replicas || dep.Status.UpdatedReplicas != infd.Spec.Replicas || dep.Status.ReadyReplicas != infd.Spec.Replicas {
		return infdPhaseProgressing, avail(metav1.ConditionFalse, infdReasonRollout, "waiting for replica count to converge")
	}
	// 7. Ready, 완전히 수렴
	//
	// 위 모든 관문을 통과했으므로 status는 최신이고, 실패도 없고, 세 count가 정확히 원하는 값과 같다.
	// 이때만 Ready를 보고하므로 이 Ready는 신뢰할 수 있다.
	return infdPhaseReady, avail(metav1.ConditionTrue, infdReasonAvailable, "all replicas ready")
}

// markDegraded: 회복 불가능한 문제를 status에 Degraded로 기록한다.
//
// Go 문법 설명:
//   - 반환 타입이 (ctrl.Result, error)인 이유는 Reconcile에서 return r.markDegraded(...) 형태로 그대로 넘기기 위해서다.
//     반환값 타입이 일치하면 이렇게 결과를 바로 전달할 수 있다.
//
// 설계 근거(설계서 Components 절): 소유권 충돌은 재시도로 풀리는 문제가 아니다.
// 사람이 남의 객체를 지우거나 이름을 바꿔 줘야만 해결되므로 에러를 반환해 백오프 재시도를 유발하면 낭비다.
// 대신 status에 남겨 운영자가 kubectl로 원인을 바로 보게 한다.
//
// 결정적 실패를 Available=False의 Degraded로 status에 반영하고,
// DeploymentConflict가 낡은 ready count를 남기지 않도록 ReadyReplicas를 0으로 초기화한다.
func (r *InferenceDeploymentReconciler) markDegraded(ctx context.Context, infd *platformv1.InferenceDeployment, reason, msg string) (ctrl.Result, error) {
	// Reconcile의 status 갱신과 같은 패턴이다.
	// 복제본에 원하는 값을 채우고, 원본과 비교해 다를 때만 Update를 보낸다.
	desired := infd.Status.DeepCopy()
	desired.Phase = infdPhaseDegraded
	// ReadyReplicas를 0으로 되돌리는 게 중요하다.
	// 충돌 상황에서는 우리 소유 Deployment가 없으니 우리가 아는 ready count는 아무 의미가 없다.
	// 이걸 0으로 안 지우면 예전에 정상이던 시절의 숫자가 그대로 남아 "Degraded인데 ReadyReplicas=2"라는 모순된 status가 된다.
	desired.ReadyReplicas = 0
	desired.ObservedGeneration = infd.Generation
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: infdCondAvailable, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: infd.Generation,
	})
	// 여기서도 변경 없으면 Update를 생략해 무한 reconcile 루프를 막는다.
	// 충돌이 계속되는 동안 이 함수는 매번 불리므로 이 가드가 특히 중요하다.
	if !equality.Semantic.DeepEqual(infd.Status, *desired) {
		infd.Status = *desired
		// infd는 이미 포인터로 받았으므로 여기서는 & 없이 그대로 넘긴다.
		if err := r.Status().Update(ctx, infd); err != nil {
			return ctrl.Result{}, fmt.Errorf("update inferencedeployment status %s/%s to Degraded: %w", infd.Namespace, infd.Name, err)
		}
	}
	// 에러 없이 정상 종료한다.
	// 문제는 status에 기록했으니 controller 입장에서는 할 일을 다 한 것이다.
	return ctrl.Result{}, nil
}

// ownedConflict: 같은 이름의 객체가 이미 있는데 그게 우리 것이 아닌지 판별한다.
// true면 "남의 것이니 손대면 안 됨"이라는 뜻이다.
//
// Go 문법 설명:
//   - obj client.Object 는 인터페이스 타입 인자다.
//     Deployment든 Service든 client.Object를 만족하므로 이 함수 하나로 두 종류를 다 처리할 수 있다.
//   - 호출한 쪽이 &appsv1.Deployment{} 또는 &corev1.Service{} 를 넘기면, 그 실제 타입이 무엇을 조회할지 결정한다.
//     클라이언트가 넘어온 값의 타입을 보고 알맞은 API 엔드포인트를 고르기 때문이다.
//
// 설계 근거(설계서 Components 절): 이 함수 하나가 워크로드 탈취를 막는 유일한 방어선이다.
// CreateOrUpdate는 이름만 맞으면 남의 객체도 그대로 고치기 때문에, 그 앞에서 반드시 이 확인이 선행되어야 한다.
//
// 주어진 이름의 객체가 존재하지만 infd가 소유하지 않는지 여부 반환
func (r *InferenceDeploymentReconciler) ownedConflict(ctx context.Context, infd *platformv1.InferenceDeployment, obj client.Object) (bool, error) {
	// types.NamespacedName{...}으로 조회할 객체의 좌표를 만든다.
	// 우리가 만드는 객체는 항상 InferenceDeployment와 같은 이름/네임스페이스를 쓴다.
	err := r.Get(ctx, types.NamespacedName{Name: infd.Name, Namespace: infd.Namespace}, obj)
	// switch { ... } 처럼 대상 값 없이 쓰면 각 case의 조건식이 참인지 차례로 검사한다.
	// if-else 체인과 같지만 여러 갈래일 때 더 읽기 좋다.
	switch {
	// apierrors.IsNotFound(err)는 "그런 객체 없음" 에러인지 판별한다.
	// 아무것도 없으면 우리가 새로 만들면 되므로 충돌이 아니다.
	case apierrors.IsNotFound(err):
		return false, nil // 없으면 충돌 아님
	// NotFound가 아닌 다른 에러(API 서버 장애 등)는 판단 자체가 불가능하다.
	// 이때 false(충돌 아님)를 반환하면 확인 없이 덮어쓰게 되므로, 에러를 위로 올려 재시도하게 한다.
	case err != nil:
		return false, err
	// 여기 도달했으면 err == nil, 즉 객체가 존재한다.
	default:
		// metav1.IsControlledBy(obj, infd)는 obj의 ownerReferences에 controller=true인 owner가 infd인지 본다.
		// 단순히 owner 목록에 있는지가 아니라 "controller" 표시가 있는 owner인지를 확인하는 게 핵심이다.
		// 우리 것이면(true) 충돌이 아니므로 ! 로 뒤집어 반환한다.
		// 즉 존재하지만 우리 소유가 아니면(false) → !false = true → 충돌이다.
		return !metav1.IsControlledBy(obj, infd), nil // 존재하나 controller owner가 아니면 충돌
	}
}

// SetupWithManager: 이 reconciler를 Manager에 등록해 실제로 이벤트를 받게 한다.
// main.go가 시작할 때 한 번 호출한다.
//
// Go 문법 설명:
//   - 아래는 "빌더 체인"이라는 패턴이며, 각 메서드가 자기 자신을 돌려주어 점(.)으로 계속 이어 붙일 수 있다.
//   - 마지막 Complete(r)에서 실제로 controller가 만들어지고 등록된다.
func (r *InferenceDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// For(...): 이 controller의 주 대상이며, InferenceDeployment가 바뀌면 Reconcile이 호출된다.
		For(&platformv1.InferenceDeployment{}).
		// Owns(...): 우리가 소유한 Deployment가 바뀌어도 Reconcile을 호출한다.
		// 이때 전달되는 Request는 바뀐 Deployment가 아니라 그 owner인 InferenceDeployment를 가리킨다.
		// Owns가 소유 여부를 판단하는 근거가 바로 위에서 SetControllerReference로 심어 둔 owner 참조다.
		//
		// 이게 없으면 Deployment의 ReadyReplicas가 올라가도 아무도 알려주지 않아,
		// status.phase가 Progressing에 멈춘 채 다음 이벤트를 기다리게 된다.
		// 또한 누가 Deployment를 몰래 고쳐도 즉시 감지해 원래대로 되돌린다(drift 복구).
		Owns(&appsv1.Deployment{}).
		// Service도 같은 이유로 감시한다.
		Owns(&corev1.Service{}).
		// Named(...): controller에 이름을 붙이며, 로그와 메트릭 라벨에 쓰인다.
		Named("inferencedeployment").
		// Complete(r): 지금까지 설정한 내용으로 controller를 만들어 Manager에 등록한다.
		// 인자 r이 실제 Reconcile 메서드를 가진 우리 reconciler다.
		Complete(r)
}
