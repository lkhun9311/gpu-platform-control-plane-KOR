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
// 그래서 suite_test.go가 만들어 두는 k8sClient 같은 값을 테스트 파일에서 import 없이 바로 쓸 수 있다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	// controller-runtime은 Reconcile을 부를 때 ctx를 넘겨주고, 매니저가 종료되면 이 ctx가 취소된다.
	"context"
	// fmt: 문자열 포매팅 표준 패키지이며, 아래 fmt.Sprintf로 condition 메시지를 만든다.
	"fmt"
	// time: 시간 단위(time.Second, time.Minute)를 쓰기 위한 표준 패키지다.
	// requeue 간격을 지정할 때 필요하다.
	"time"

	// corev1: 쿠버네티스 기본(core) API 그룹의 타입들이다(Namespace, ResourceQuota 등).
	// 별칭 corev1은 "core API 그룹의 v1 버전"이라는 뜻이며, 여러 v1을 구분하려고 붙인다.
	corev1 "k8s.io/api/core/v1"
	// equality: 쿠버네티스 객체를 "의미상 같은지" 비교하는 도구다.
	// 아래 equality.Semantic.DeepEqual에서 쓴다.
	"k8s.io/apimachinery/pkg/api/equality"
	// apierrors: API 서버가 준 에러의 종류를 판별하는 헬퍼다(IsNotFound, IsAlreadyExists 등).
	// 표준 errors 패키지와 이름이 겹치므로 apierrors라는 별칭을 붙였다.
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	// meta: status condition 목록을 다루는 헬퍼다.
	// 아래 meta.SetStatusCondition에서 쓴다.
	"k8s.io/apimachinery/pkg/api/meta"
	// resource: CPU/메모리/GPU 같은 수량을 표현하는 Quantity 타입을 제공한다.
	// ResourceQuota의 상한 값은 숫자가 아니라 이 Quantity 타입이어야 한다.
	"k8s.io/apimachinery/pkg/api/resource"
	// metav1: 모든 쿠버네티스 객체가 공유하는 메타데이터 타입들이다(ObjectMeta, Condition, Time 등).
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// runtime: 타입 등록표인 Scheme을 제공한다.
	// Scheme이 있어야 Go 타입과 쿠버네티스 GroupVersionKind를 서로 변환할 수 있다.
	"k8s.io/apimachinery/pkg/runtime"
	// types: NamespacedName(이름+namespace 쌍)처럼 객체를 가리키는 키 타입을 제공한다.
	"k8s.io/apimachinery/pkg/types"
	// ctrl: controller-runtime의 최상위 패키지이며 짧게 쓰려고 ctrl로 별칭을 붙이는 게 관례다.
	// ctrl.Request, ctrl.Result, ctrl.NewControllerManagedBy 등이 여기서 온다.
	ctrl "sigs.k8s.io/controller-runtime"
	// client: 쿠버네티스 객체를 읽고 쓰는 클라이언트 인터페이스다.
	// client.IgnoreNotFound 같은 헬퍼도 여기 있다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// controllerutil: finalizer 조작과 owner reference 설정 헬퍼 모음이다.
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	// logf: controller-runtime의 로깅 진입점이며, ctx에 심어 둔 logger를 꺼내 쓸 때 필요하다.
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(GPUQuotaPolicy 등)이다.
	// 원래 패키지 이름은 v1이지만 위의 corev1/metav1과 헷갈리지 않도록 platformv1로 부른다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// const 블록: 이 파일 전체에서 쓰는 고정값(상수)들을 한 번에 선언한다.
//
// Go 문법 설명:
//   - const는 컴파일 시점에 값이 정해지는 상수 선언 키워드이며, 실행 중에 바뀔 수 없다.
//   - 괄호로 묶으면 여러 상수를 한 번에 나열할 수 있다.
//   - 이름이 소문자로 시작하면 이 패키지(controller) 안에서만 보이는 비공개 상수다.
//     여기 값들은 이 controller의 내부 규약이므로 전부 비공개로 둔다.
//   - 타입을 안 적으면 "타입 없는 상수(untyped constant)"가 되어 쓰이는 자리에 맞춰 타입이 정해진다.
const (
	// 삭제 전에 정리할 게 있다는 표식(finalizer)이다.
	// 이 표식이 붙어 있으면 Kubernetes는 object를 실제로 지우지 않고 DeletionTimestamp만 채운 채 기다린다.
	// reconciler가 동기화해 둔 ResourceQuota를 먼저 지운 뒤 표식을 떼어야 실제 삭제가 진행된다.
	// 이렇게 해야 정책만 사라지고 quota는 남는 고아(orphan) 상태를 막을 수 있다.
	// envtest에는 garbage collection이 없어 owner reference에만 기대면 정리가 안 되므로 정리를 코드로 직접 해야 한다.
	// finalizer 이름은 다른 컨트롤러의 것과 겹치지 않도록 도메인 형식(<리소스>.<그룹>/finalizer)으로 짓는 게 관례다.
	gpuQuotaFinalizer = "gpuquotapolicy.platform.lkhun9311.github.io/finalizer"

	// ResourceQuota가 정책과 맞는지 여부를 보고하는 condition의 타입 이름이다.
	// condition은 "Type/Status/Reason/Message"로 이루어진 표준 상태 보고 단위이며, 사람과 도구가 함께 읽는다.
	conditionSynced = "Synced"
	// reasonQuotaSynced: 동기화가 성공했을 때 condition에 적는 기계 판독용 사유 코드다.
	// Reason은 사람이 읽는 Message와 달리 CamelCase 한 단어여야 한다는 API 규약이 있다.
	reasonQuotaSynced = "QuotaSynced"
	// reasonQuotaConflict: 남의 ResourceQuota와 이름이 충돌해 동기화를 포기했을 때 적는 사유 코드다.
	reasonQuotaConflict = "QuotaConflict"

	// 상위 수준 진행 상태를 한 단어로 요약한 phase다.
	// ResourceQuota가 정책 상한과 일치하면 Synced이고 결정적 실패(남의 ResourceQuota와 이름 충돌 등)면 Degraded다.
	// 일시적 API 오류는 phase에 반영하지 않고 requeue만 하므로 재시도해도 phase가 흔들리지 않는다.
	// 이 phase는 이 controller만 소유하므로 NodeHealth controller는 자기 phase를 독립적으로 정할 수 있다.
	phaseSynced   = "Synced"
	phaseDegraded = "Degraded"

	// GPU 소비를 제한하는 ResourceQuota key다.
	// 확장 resource(nvidia.com/gpu)는 quota에서 requests.<resource> 형태의 key로 추적하며,
	// local에서는 시뮬레이션한 nvidia.com/gpu 용량을 이 key로 제한한다.
	//
	// Go 문법 설명:
	//   - corev1.ResourceName("...")은 형 변환(type conversion)이다.
	//   - ResourceName은 string을 바탕으로 정의된 별도 타입이라 문자열을 그대로 대입할 수 없고 이렇게 감싸야 한다.
	//   - 이렇게 이름 있는 타입을 쓰면 아무 문자열이나 실수로 넘기는 것을 컴파일러가 막아 준다.
	gpuRequestsResource = corev1.ResourceName("requests.nvidia.com/gpu")
)

// GPUQuotaPolicyReconciler: GPUQuotaPolicy를 관찰해 실제 ResourceQuota를 원하는 상한으로 맞춰 가는 controller다.
//
// Go 문법 설명:
//   - type 이름 struct { ... }는 여러 필드를 묶는 사용자 정의 타입(구조체) 선언이다.
//   - client.Client처럼 필드 이름 없이 타입만 적은 것을 "임베딩(embedding)"이라고 한다.
//     임베딩하면 그 타입의 메서드가 이 구조체의 메서드처럼 승격되어, r.Get(...)/r.Update(...)를 r.Client.Get(...) 없이 바로 쓸 수 있다.
//     아래 Reconcile 안의 r.Get, r.Create, r.Delete, r.Status()가 전부 이 임베딩 덕분에 가능한 호출이다.
//   - Scheme *runtime.Scheme는 이름이 있는 일반 필드이며, 별표(*)는 포인터라는 뜻이다.
//     Scheme은 owner reference를 걸 때 "GPUQuotaPolicy의 Kind가 뭔지"를 알아내는 데 쓰이므로 반드시 주입돼야 한다.
//   - 대문자로 시작하는 이름은 패키지 밖에서도 보이는 공개(export) 이름이다.
//     이 타입은 cmd/main.go에서 만들어 매니저에 등록하므로 공개여야 한다.
type GPUQuotaPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// 아래 marker는 코드 생성기(controller-gen)가 읽어 RBAC 권한을 자동 생성하므로 문구를 바꾸지 말 것
// 주석처럼 보이지만 사실상 코드이며, 여기 적힌 groups/resources/verbs가 그대로 ClusterRole YAML로 변환된다.
// 정책 본체는 읽고 쓰며, status와 finalizers는 별도 subresource라 권한을 따로 선언해야 한다.
// groups=""는 core API 그룹(그룹 이름이 빈 문자열)이라는 뜻이고, ResourceQuota를 만들고 지우려면 이 줄이 필요하다.
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=gpuquotapolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete

// Reconcile: 정책이 바뀔 때마다 불려 대상 namespace의 ResourceQuota를 원하는 상한으로 맞춘다.
// 누가 손대서 어긋나면 다시 맞추며 정책이 지워지면 ResourceQuota도 지운다.
// 몇 번을 다시 불려도 같은 결과가 나오도록 멱등(idempotent)하게 설계한다.
//
// Go 문법 설명:
//   - func (r *GPUQuotaPolicyReconciler) 부분은 리시버(receiver)다.
//     이 함수가 GPUQuotaPolicyReconciler 타입에 붙는 "메서드"라는 뜻이다.
//   - *GPUQuotaPolicyReconciler처럼 별표(*)를 붙이면 값 복사가 아니라 원본을 가리키는 포인터를 받는다.
//   - ctrl.Request는 "어떤 객체를 재조정하라"는 요청이며, 안에 NamespacedName(이름+namespace)만 들어 있다.
//     객체 본문이 아니라 이름만 오는 이유는, 큐에 쌓인 사이 객체가 또 바뀌었을 수 있어 항상 최신본을 직접 읽어야 하기 때문이다.
//   - ctrl.Result는 "다음에 언제 또 부를지"를 controller-runtime에 알려주는 반환값이다.
//     빈 값 ctrl.Result{}는 "다시 부를 필요 없음"이고, RequeueAfter를 채우면 그 시간 뒤에 다시 불린다.
//   - 반환값이 (ctrl.Result, error) 두 개인 이유는, 에러를 nil이 아닌 값으로 돌려주면
//     controller-runtime이 지수 백오프(exponential backoff)로 알아서 재시도해 주기 때문이다.
//     그래서 일시적 오류는 직접 재시도 루프를 돌지 않고 그냥 에러로 반환하는 게 이 프레임워크의 관용구다.
//
// 재조정 흐름:
//  1. 삭제 중이면 동기화해 둔 ResourceQuota를 지운 뒤 finalizer를 떼어 실제 삭제가 진행되게 한다,
//  2. finalizer가 없으면 붙이고 이번 pass를 끝낸다,
//  3. 정책의 GPUCount로 원하는 상한(requests.nvidia.com/gpu)을 계산한다,
//  4. ResourceQuota가 없으면 만들고, 남의 소유면 Degraded로 보고하며, drift가 있으면 원하는 값으로 되돌린다,
//  5. Synced phase와 condition을 status에 멱등하게 기록한다.
//
// 왜 이 순서인가:
//   - 삭제 처리를 맨 앞에 두는 이유는, 삭제 중인 객체에 대고 ResourceQuota를 다시 만들면 지운 걸 되살리는 꼴이 되기 때문이다.
//   - finalizer 부착을 소유 resource 생성보다 앞에 두는 이유는, 그 사이에 삭제 요청이 들어와도 정리가 보장되게 하기 위해서다.
//   - status 기록을 맨 뒤에 두는 이유는, 실제 동기화가 성공한 뒤에만 Synced라고 보고해야 status가 거짓말을 하지 않기 때문이다.
func (r *GPUQuotaPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// ctx 안에 controller-runtime이 심어 둔 logger를 꺼낸다.
	// 이렇게 꺼내 쓰면 로그에 controller 이름과 대상 객체 이름이 자동으로 붙어 추적이 쉬워진다.
	log := logf.FromContext(ctx)

	// 이번에 바뀐 정책을 읽고,
	// 이미 삭제됐으면 IgnoreNotFound가 조용히 끝내준다.
	//
	// Go 문법 설명:
	//   - var policy platformv1.GPUQuotaPolicy는 제로값으로 초기화된 빈 구조체 변수를 만든다.
	//   - &policy의 &는 주소(포인터)를 넘긴다는 뜻이며, Get이 이 변수를 채워야 하므로 포인터가 필요하다.
	//   - if err := ...; err != nil은 호출과 동시에 err를 만들고 즉시 검사하는 Go의 관용구이며, 이 err는 if 블록 안에서만 유효하다.
	//   - client.IgnoreNotFound(err)는 err가 "없음(404)" 에러면 nil을, 그 밖의 에러면 err를 그대로 돌려준다.
	//     즉 "이미 지워진 객체"는 에러 없이 종료하고, 진짜 장애만 에러로 올려 재시도시키는 한 줄짜리 분기다.
	//     정책이 지워진 뒤에도 큐에 요청이 남아 Reconcile이 불릴 수 있으므로 이 처리가 반드시 필요하다.
	var policy platformv1.GPUQuotaPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 이 정책에 대응하는 ResourceQuota의 위치를 미리 계산한다.
	// types.NamespacedName{...}은 이름과 namespace를 묶어 객체 하나를 가리키는 키다.
	// GPUQuotaPolicy는 cluster-scoped라 자기 namespace가 없으므로, 목적지 namespace는 spec.targetNamespace에서 가져온다.
	rqKey := types.NamespacedName{Name: quotaName(policy.Name), Namespace: policy.Spec.TargetNamespace}

	// 삭제 중이면 우리가 만든 ResourceQuota부터 지운 뒤 표식을 떼어 실제 삭제가 진행되게 한다.
	//
	// Go 문법 설명:
	//   - DeletionTimestamp는 *metav1.Time이며, 삭제 요청이 들어오기 전에는 비어 있다.
	//   - IsZero()는 "값이 비었는지"를 알려주므로, !IsZero()는 "삭제 요청이 들어왔다"는 뜻이 된다.
	//   - 즉 쿠버네티스에서 finalizer가 붙은 객체의 삭제는 즉시 사라지는 게 아니라 이 타임스탬프가 찍히는 것으로 시작한다.
	if !policy.DeletionTimestamp.IsZero() {
		// 우리 finalizer가 붙어 있을 때만 정리한다.
		// 이미 떼어 낸 뒤 또 불린 경우(중복 호출)에는 아무것도 하지 않고 끝나야 멱등하다.
		if controllerutil.ContainsFinalizer(&policy, gpuQuotaFinalizer) {
			var rq corev1.ResourceQuota
			// switch err := ...; { ... } 는 "조건 없는 switch"이며, 각 case에 불리언 식을 적어 if-else 사슬처럼 쓴다.
			// 세미콜론 앞에서 err를 만들고, 이 err는 switch 블록 전체에서 쓸 수 있다.
			// Get 결과를 "성공 / 이미 없음 / 진짜 에러" 세 갈래로 나누기에 if-else보다 읽기 좋다.
			switch err := r.Get(ctx, rqKey, &rq); {
			case err == nil:
				// 이미 없으면 성공으로 보고 그 밖의 삭제 오류만 실패로 취급한다.
				// Get과 Delete 사이에 남이 먼저 지웠을 수 있으므로(경쟁 상태) IsNotFound는 성공으로 흡수한다.
				// &&는 "그리고(AND)"이며, 앞 조건이 거짓이면 뒤는 평가하지 않는다(short-circuit).
				if err := r.Delete(ctx, &rq); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
				// log.Info의 두 번째 인자부터는 "키, 값, 키, 값..." 쌍으로 넘기는 구조적 로깅 방식이다.
				log.Info("Deleted synced ResourceQuota on deletion", "resourceQuota", rqKey.String())
			case apierrors.IsNotFound(err):
				// 이미 사라져 지울 게 없음
				// 아무것도 하지 않고 아래 finalizer 제거로 넘어간다.
			default:
				// 그 밖의 에러(API 서버 장애 등)는 일시적일 수 있으므로 위로 올려 백오프 재시도를 맡긴다.
				// 여기서 finalizer를 떼어 버리면 정리되지 않은 ResourceQuota가 영영 남으므로 절대 진행하면 안 된다.
				return ctrl.Result{}, err
			}
			// 정리가 끝났으니 표식을 떼어 저장한다.
			// RemoveFinalizer는 메모리 상의 객체에서만 표식을 지우므로, r.Update로 서버에 반영해야 실제 삭제가 이어진다.
			// 이 Update가 성공하는 순간 finalizer가 0개가 되어 API 서버가 객체를 진짜로 제거한다.
			controllerutil.RemoveFinalizer(&policy, gpuQuotaFinalizer)
			if err := r.Update(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		// 삭제 경로는 여기서 끝난다.
		// 빈 ctrl.Result{}와 nil 에러는 "성공했고 다시 부를 필요 없음"을 뜻한다.
		return ctrl.Result{}, nil
	}

	// 표식이 없으면 먼저 붙이고 이번 pass는 종료하며,
	// 소유 resource를 만들기 전에 정리 약속을 먼저 걸어야 중간에 삭제돼도 정리가 보장된다.
	// 곧바로 이어서 동기화하지 않고 return하는 이유는, Update로 ResourceVersion이 올라가 손에 든 policy 사본이 낡았기 때문이다.
	// 낡은 사본으로 status를 쓰면 conflict가 나므로, Update가 유발하는 다음 watch event에서 최신본으로 다시 시작하는 편이 안전하다.
	if !controllerutil.ContainsFinalizer(&policy, gpuQuotaFinalizer) {
		controllerutil.AddFinalizer(&policy, gpuQuotaFinalizer)
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 원하는 상한 계산은 정책의 GPUCount만큼을 requests.nvidia.com/gpu로 제한하며,
	// NOTE spec.gpuClass는 아직 class별로 시행하지 않고,
	// 이 milestone은 class와 무관하게 단일 key(requests.nvidia.com/gpu)만 제한하므로,
	// 같은 namespace를 겨냥한 gpuClass가 다른 두 정책이 같은 key를 제한하면,
	// Kubernetes가 quota를 AND로 합쳐 가장 엄격한 쪽이 이기고,
	// class별 key 분리는 시뮬레이션 용량 modeling 방식에 달려 보류하며 gpuClass는 기록만 하고 quota 범위는 안 정한다.
	//
	// Go 문법 설명:
	//   - corev1.ResourceList는 map[ResourceName]Quantity, 즉 "자원 이름 → 수량" 해시맵이다.
	//   - resource.NewQuantity(값, 형식)은 Quantity의 포인터를 돌려주므로, 앞에 별표(*)를 붙여 값 자체를 꺼내 map에 넣는다.
	//   - int64(policy.Spec.Limits.GPUCount)는 형 변환이며, GPUCount는 int32라 NewQuantity가 요구하는 int64로 바꿔야 한다.
	//   - resource.DecimalSI는 "8", "1k"처럼 10진수로 표기하라는 형식 지정이다(메모리에 쓰는 BinarySI와 대비된다).
	//
	// 이 값은 "원하는 상태(desired state)"이며, 아래에서 실제 상태와 비교해 차이가 있을 때만 고친다.
	// 매 reconcile마다 spec에서 새로 계산하므로 정책의 GPUCount가 바뀌면 자동으로 반영된다.
	desiredHard := corev1.ResourceList{
		gpuRequestsResource: *resource.NewQuantity(int64(policy.Spec.Limits.GPUCount), resource.DecimalSI),
	}

	// 실제 상태를 읽어 원하는 상태와 맞춰 간다.
	// 여기서도 "없음 / 진짜 에러 / 이미 있음" 세 갈래를 조건 없는 switch로 나눈다.
	var rq corev1.ResourceQuota
	switch err := r.Get(ctx, rqKey, &rq); {
	case apierrors.IsNotFound(err):
		// 없으면 새로 만든다.
		// 이 갈래가 drift recovery의 핵심이다(누가 ResourceQuota를 지워도 다음 reconcile에서 되살아난다).
		// rq는 위에서 var로 선언해 둔 변수이므로, 여기서는 := 가 아니라 = 로 통째로 덮어쓴다.
		rq = corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: rqKey.Name, Namespace: rqKey.Namespace},
			Spec:       corev1.ResourceQuotaSpec{Hard: desiredHard},
		}
		// 소유 참조를 걸어 정책이 지워질 때 이 ResourceQuota도 함께 정리되게 한다.
		// SetControllerReference는 metadata.ownerReferences에 "이 정책이 controller다"라는 항목을 넣는다.
		// Scheme이 필요한 이유는 GPUQuotaPolicy의 apiVersion/kind를 등록표에서 찾아 적어야 하기 때문이다.
		// 이 참조는 실제 클러스터의 garbage collection뿐 아니라, 아래 IsControlledBy 소유권 판정과 SetupWithManager의 Owns watch에도 함께 쓰인다.
		if err := controllerutil.SetControllerReference(&policy, &rq, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &rq); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// 경쟁에서 밀림(동시 reconcile이나 informer 지연),
				// 이제는 객체가 있으므로 실패시키지 않고 1초 뒤 다시 reconcile해 정상 경로로 흡수한다.
				// 캐시가 아직 낡아 Get은 없다고 했는데 Create는 있다고 하는 상황이며, 잠깐 기다리면 캐시가 따라잡는다.
				// ctrl.Result{RequeueAfter: time.Second}는 "에러는 아니지만 1초 뒤 다시 불러 달라"는 뜻이다.
				// 에러로 올리지 않는 이유는, 이건 장애가 아니라 예상된 경쟁 상황이라 로그를 시끄럽게 만들 필요가 없기 때문이다.
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		log.Info("Created ResourceQuota", "resourceQuota", rqKey.String())
	case err != nil:
		// IsNotFound가 아닌 에러이므로 읽기 자체가 실패한 것이다.
		// 실제 상태를 모르는 채로 고치면 위험하므로 아무것도 하지 않고 에러를 올려 재시도를 맡긴다.
		return ctrl.Result{}, err
	default:
		// 이미 있으면 이 정책이 소유한 게 맞는지부터 확인하고,
		// 우리가 안 만든 남의 ResourceQuota를 덮어쓰면 남의 quota를 훼손하므로 가로채지 않고 Degraded로 보고한다.
		// IsControlledBy는 rq의 ownerReferences에 controller=true인 항목이 있고 그게 이 policy인지 확인한다.
		if !metav1.IsControlledBy(&rq, &policy) {
			log.Info("ResourceQuota exists but is not owned by this policy; refusing to overwrite",
				"resourceQuota", rqKey.String())
			// markDegraded가 (ctrl.Result, error)를 그대로 돌려주므로 반환값을 그대로 전달한다.
			// fmt.Sprintf는 %s 자리에 값을 끼워 넣어 사람이 읽을 수 있는 condition 메시지를 만든다.
			return r.markDegraded(ctx, &policy, reasonQuotaConflict,
				fmt.Sprintf("ResourceQuota %s already exists and is not owned by this policy", rqKey.String()))
		}
		// 소유가 맞고 상한이 어긋났으면(drift) 원하는 값으로 되돌린다.
		//
		// Go 문법 설명:
		//   - equality.Semantic.DeepEqual은 쿠버네티스 전용 깊은 비교이며, 표준 reflect.DeepEqual과 달리 "의미상 같음"을 본다.
		//   - Quantity는 같은 수량이라도 내부 캐시 문자열이 달라 reflect.DeepEqual이 false를 낼 수 있다.
		//     예컨대 서버가 돌려준 "8"과 우리가 만든 8은 표현이 달라도 같은 값이며, Semantic만 이를 같다고 판정한다.
		//   - 그래서 여기서 표준 비교를 쓰면 매번 drift로 오판해 무한히 Update를 날리게 된다.
		if !equality.Semantic.DeepEqual(rq.Spec.Hard, desiredHard) {
			rq.Spec.Hard = desiredHard
			if err := r.Update(ctx, &rq); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("Corrected ResourceQuota drift", "resourceQuota", rqKey.String())
		}
		// 소유가 맞고 값도 같으면 아무것도 하지 않는다.
		// 이 "차이가 있을 때만 쓴다"는 규칙이 멱등성의 핵심이며, 정상 상태에서 ResourceVersion이 오르지 않게 해 준다.
	}

	// 동기화 성공을 status에 멱등하게 기록하되,
	// 복사본에 원하는 값을 채운 뒤 실제로 달라졌을 때만 저장한다.
	//
	// Go 문법 설명:
	//   - DeepCopy()는 controller-gen이 자동 생성해 준 메서드로, 내부 슬라이스/맵까지 통째로 복제한 새 포인터를 준다.
	//     그냥 대입하면 Conditions 슬라이스를 원본과 공유해, 비교하기도 전에 원본이 함께 바뀌어 버린다.
	//   - ObservedGeneration에 policy.Generation을 넣는 이유는 "몇 번째 spec을 보고 판단한 status인지" 남기기 위해서다.
	//     Generation은 spec이 바뀔 때만 오르므로, 둘이 같으면 이 status가 최신 spec 기준이라는 뜻이다.
	desired := policy.Status.DeepCopy()
	desired.ObservedGeneration = policy.Generation
	setQuotaPhase(desired, phaseSynced)
	// meta.SetStatusCondition은 같은 Type의 condition이 있으면 갱신하고 없으면 추가하는 upsert 헬퍼다.
	// 중요한 점은 Status 값이 실제로 바뀔 때만 LastTransitionTime을 새로 찍는다는 것이다.
	// 그래서 매번 호출해도 조건이 그대로면 결과가 같아, 아래 DeepEqual 비교가 "변화 없음"으로 나온다.
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               conditionSynced,
		Status:             metav1.ConditionTrue,
		Reason:             reasonQuotaSynced,
		Message:            "ResourceQuota synced from policy",
		ObservedGeneration: policy.Generation,
	})

	// 바뀐 게 있을 때만 저장하며 매번 쓰면 불필요한 event와 충돌이 늘어난다.
	// 무조건 Status().Update를 부르면 watch event가 또 발생해 Reconcile이 다시 불리는 무한 루프가 되기 쉽다.
	// *desired는 포인터가 가리키는 값을 꺼내는 역참조이며, policy.Status가 값 타입이라 값끼리 비교하고 값을 대입한다.
	if !equality.Semantic.DeepEqual(policy.Status, *desired) {
		policy.Status = *desired
		// r.Status()는 status subresource 전용 writer를 준다.
		// 본체 Update가 아니라 이걸 써야 spec을 건드리지 않고 status만 갱신되며, RBAC도 status용 권한만 필요하다.
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Updated GPUQuotaPolicy status", "name", policy.Name, "phase", desired.Phase)
	}

	// 여기까지 왔으면 정상 동기화 완료다.
	// 빈 Result와 nil 에러는 "재시도도 재예약도 필요 없음"을 뜻하며, 다음 호출은 watch event가 있을 때만 일어난다.
	return ctrl.Result{}, nil
}

// quotaName: 정책 이름으로 ResourceQuota 이름을 항상 같은 규칙으로 만들며,
// 같은 정책은 늘 같은 이름으로 mapping돼 중복 생성이나 추적 혼선을 막는다.
//
// Go 문법 설명:
//   - 리시버가 없는 일반 함수다(특정 타입에 붙지 않음).
//   - 소문자로 시작하므로 이 패키지 안에서만 보이는 비공개 함수다.
//   - 문자열끼리 +로 이으면 새 문자열이 만들어진다.
//
// 이 함수가 순수 함수(같은 입력이면 항상 같은 출력)라는 점이 중요하다.
// 이름을 매번 새로 짓거나 랜덤 접미사를 붙이면 reconcile마다 다른 ResourceQuota가 생겨 멱등성이 깨진다.
func quotaName(policyName string) string {
	return "gpuquota-" + policyName
}

// markDegraded: 결정적 실패를 Synced=False condition과 Degraded phase로 status에 기록하고,
// 막힌 조건이 풀리면 스스로 복구되도록 RequeueAfter로 재확인을 예약하며,
// Owns watch는 우리가 소유하지 않은 ResourceQuota 변화에는 안 울리므로 시간 기반 재확인이 필요하다.
//
// Go 문법 설명:
//   - (ctx context.Context, policy *platformv1.GPUQuotaPolicy, reason, msg string)에서 reason과 msg는 둘 다 string이며, 같은 타입이 이어지면 타입을 마지막에 한 번만 적어도 된다.
//   - policy를 포인터로 받는 이유는 아래에서 policy.Status를 실제로 바꿔야 하기 때문이다(값으로 받으면 복사본만 바뀐다).
//   - 반환 타입이 Reconcile과 똑같은 (ctrl.Result, error)라서 호출부에서 return r.markDegraded(...)로 그대로 넘길 수 있다.
//
// 왜 에러를 반환하지 않는가:
//   - 이름 충돌은 재시도한다고 저절로 풀리는 문제가 아니라 사람이 개입해야 하는 결정적 실패다.
//   - 에러로 올리면 controller-runtime이 백오프로 계속 재시도하며 로그만 시끄러워지므로, 대신 status로 보고하고 1분 뒤 한 번만 확인한다.
func (r *GPUQuotaPolicyReconciler) markDegraded(ctx context.Context, policy *platformv1.GPUQuotaPolicy, reason, msg string) (ctrl.Result, error) {
	// 성공 경로와 똑같이 "복사본에 원하는 값을 채우고 달라졌을 때만 저장하는" 패턴을 쓴다.
	desired := policy.Status.DeepCopy()
	desired.ObservedGeneration = policy.Generation
	setQuotaPhase(desired, phaseDegraded)
	// Status를 ConditionFalse로 두고 Reason에 왜 실패했는지 기계 판독용 코드를, Message에 사람이 읽을 설명을 넣는다.
	meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type:               conditionSynced,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: policy.Generation,
	})
	// 매 1분마다 같은 status를 다시 쓰지 않도록 여기서도 변화가 있을 때만 저장한다.
	if !equality.Semantic.DeepEqual(policy.Status, *desired) {
		policy.Status = *desired
		// 여기서는 policy가 이미 포인터라 &를 붙이지 않는다(Reconcile 안에서는 값 변수라 &policy로 넘겼다).
		if err := r.Status().Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	// 1분 뒤 다시 reconcile 예약
	// 충돌하던 남의 ResourceQuota가 사라지면 그때 정상 경로로 돌아가 Synced가 되므로 자가 복구가 된다.
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// setQuotaPhase: phase를 바꾸되 값이 실제로 달라질 때만 전환 시각을 갱신하며,
// 같은 phase를 다시 써도 전환 시각이 튀지 않게 하려는 것이다.
//
// Go 문법 설명:
//   - status *platformv1.GPUQuotaPolicyStatus를 포인터로 받아야 호출한 쪽의 값이 실제로 바뀐다.
//   - 반환값이 없으므로 함수 시그니처 뒤에 타입을 적지 않는다.
//   - return을 값 없이 쓰면 "여기서 함수를 끝내라"는 뜻이며, 아래 코드를 건너뛰는 early return 관용구다.
//   - metav1.Now()는 현재 시각을 값으로 주고, &now로 주소를 얻는 이유는 LastTransitionTime이 *metav1.Time 포인터 필드이기 때문이다.
//     포인터 필드인 이유는 "아직 한 번도 전환한 적 없음"을 nil로 표현하기 위해서다.
//     metav1.Now()를 곧바로 &metav1.Now()처럼 쓸 수 없어서 now 변수를 한 번 거친다(함수 반환값에는 주소를 못 얻는다).
//
// 이 early return이 없으면 reconcile마다 시각이 바뀌어 DeepEqual이 항상 다르다고 판정한다.
// 그러면 Status().Update가 매번 실행되고, 그게 watch event를 만들어 Reconcile을 또 부르는 무한 루프가 된다.
func setQuotaPhase(status *platformv1.GPUQuotaPolicyStatus, phase string) {
	if status.Phase == phase {
		return
	}
	status.Phase = phase
	now := metav1.Now()
	status.LastTransitionTime = &now
}

// SetupWithManager: 이 controller를 Manager에 등록해 무엇을 지켜보고 무엇을 소유하는지 알려주며,
// For는 GPUQuotaPolicy가 바뀌면 reconcile하고 Owns는 우리가 만든 ResourceQuota가 바뀌어도 reconcile한다.
//
// Go 문법 설명:
//   - 점(.)을 줄 끝에 두고 다음 줄로 이어 가는 방식을 메서드 체이닝(builder pattern)이라고 한다.
//     각 메서드가 빌더 자신을 다시 돌려주므로 이렇게 계속 이어 붙일 수 있다.
//   - &platformv1.GPUQuotaPolicy{}처럼 빈 객체의 포인터를 넘기는 이유는, 값이 아니라 "타입"만 알려주면 되기 때문이다.
//     빌더는 이 빈 값에서 타입 정보만 뽑아 어떤 종류의 객체를 watch할지 정한다.
//   - Complete(r)이 실제로 controller를 만들어 Manager에 등록하며, 실패하면 error를 돌려준다.
//     그래서 이 함수의 반환 타입이 error 하나이고, cmd/main.go가 이 에러를 보고 기동을 중단한다.
//
// 각 단계의 의미:
//   - For(GPUQuotaPolicy): 이 controller의 주 대상이며, 정책이 생기거나 바뀌거나 지워지면 그 이름으로 Reconcile이 큐에 들어간다.
//   - Owns(ResourceQuota): owner reference가 이 정책을 가리키는 ResourceQuota가 바뀌면, 그 owner인 정책 이름으로 Reconcile이 큐에 들어간다.
//     drift recovery가 시간이 아니라 event로 즉시 동작하는 이유가 바로 이 줄이다(누가 quota를 지우거나 고치면 바로 불린다).
//     반대로 소유하지 않은 ResourceQuota의 변화는 여기에 안 걸리므로, markDegraded가 RequeueAfter로 따로 재확인한다.
//   - Named("gpuquotapolicy"): controller 이름이며 로그와 메트릭 label에 쓰이고, 같은 Manager 안에서 유일해야 한다.
func (r *GPUQuotaPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1.GPUQuotaPolicy{}).
		Owns(&corev1.ResourceQuota{}).
		Named("gpuquotapolicy").
		Complete(r)
}
