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
// 그래서 reconcile_helpers.go의 setPhase나 nodehealth_controller.go의 NodeHealthReconciler를 import 없이 바로 쓸 수 있다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	// 쿠버네티스 클라이언트 호출은 매니저가 종료될 때 진행 중인 작업을 끊을 수 있도록 전부 ctx를 첫 인자로 받는다.
	"context"

	// runtime: 쿠버네티스 오브젝트의 직렬화/역직렬화 규칙을 담은 Scheme 타입이 들어 있다.
	// Scheme는 "Go 타입 ↔ GroupVersionKind" 대응표라고 보면 된다.
	"k8s.io/apimachinery/pkg/runtime"
	// ctrl: sigs.k8s.io/controller-runtime의 별칭(alias)이다.
	// 원래 패키지 이름은 controllerruntime이라 길어서 관례적으로 ctrl로 줄여 부른다.
	// ctrl.Request, ctrl.Result, ctrl.Manager 등 컨트롤러 골격에 필요한 타입이 여기 있다.
	ctrl "sigs.k8s.io/controller-runtime"
	// client: 쿠버네티스 API를 읽고 쓰는 클라이언트 인터페이스(client.Client)를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// logf: controller-runtime의 구조화 로깅 도우미이며, ctx에 실려 온 로거를 꺼내 쓴다.
	// 별칭 logf를 쓰는 이유는 표준 라이브러리 log와 이름이 겹치지 않게 하기 위해서다.
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(MLTrainingJob 등)이다.
	// import 경로 앞의 platformv1은 별칭이다.
	// 원래 패키지 이름은 v1이지만 다른 v1들(metav1, corev1 등)과 헷갈리지 않도록 platformv1이라는 이름으로 부른다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// MLTrainingJobReconciler: MLTrainingJob object를 reconcile하는 컨트롤러 본체다.
// reconcile이란 "선언된 spec(원하는 상태)과 클러스터의 실제 상태를 비교해 실제를 원하는 쪽으로 밀어붙이는 것"이다.
//
// Go 문법 설명:
//   - type 이름 struct { ... } 는 여러 필드를 묶는 사용자 정의 타입(구조체) 선언이다.
//   - client.Client 처럼 필드 이름 없이 타입만 적은 것을 "임베딩(embedding)"이라고 부른다.
//     임베딩하면 그 타입의 메서드가 바깥 타입의 메서드처럼 승격되어, r.Get(...)이나 r.Status()를 r.Client.Get(...) 없이 바로 쓸 수 있다.
//   - Scheme *runtime.Scheme 처럼 별표(*)를 붙이면 "runtime.Scheme의 포인터"라는 뜻이다.
//     Scheme는 매니저가 만든 인스턴스 하나를 공유해야 하므로 값 복사가 아닌 포인터로 들고 있는다.
//   - 대문자로 시작하는 이름(MLTrainingJobReconciler, Scheme)은 패키지 밖에서도 보이는 "공개(export)"다.
//     cmd/main.go에서 이 타입을 만들어 매니저에 등록해야 하므로 공개여야 한다.
type MLTrainingJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// 아래 세 줄은 주석처럼 보이지만 실제로는 코드 생성 지시자(kubebuilder 마커)다.
// make manifests가 이 마커를 읽어 config/rbac 아래의 ClusterRole YAML을 생성하므로,
// 문구를 번역하거나 수정하면 컨트롤러가 실제로 필요한 권한을 잃는다.
// 각각 (1)MLTrainingJob 본체, (2)status 서브리소스, (3)finalizers 서브리소스에 대한 권한을 선언한다.
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.lkhun9311.github.io,resources=mltrainingjobs/finalizers,verbs=update

// Reconcile: cluster의 현재 상태를 원하는 상태에 가깝게 옮기는 main Kubernetes reconciliation loop의 일부다.
// controller-runtime이 MLTrainingJob에 변화가 생길 때마다 이 메서드를 대신 호출해 준다.
// TODO(user): MLTrainingJob object가 지정한 상태와 실제 cluster 상태를 비교하고,
// 사용자가 지정한 상태를 cluster가 반영하도록 동작을 수행하게끔 Reconcile 함수를 수정할 것
//
// Go 문법 설명:
//   - func (r *MLTrainingJobReconciler) 부분은 리시버(receiver)다.
//     이 함수가 MLTrainingJobReconciler 타입에 붙는 "메서드"라는 뜻이고, 호출은 reconciler.Reconcile(...) 형태가 된다.
//   - ctrl.Request는 "어떤 오브젝트를 재조정하라"는 요청이며, 안에는 NamespacedName(이름 + 네임스페이스)만 들어 있다.
//     오브젝트 자체가 아니라 이름만 오는 이유는, 요청이 큐에 머무는 동안 오브젝트가 이미 바뀌었을 수 있어서다.
//     그래서 컨트롤러는 항상 이름으로 최신 상태를 다시 읽어야 한다.
//   - ctrl.Result는 "이 요청을 다시 큐에 넣을지"를 컨트롤러 런타임에 알리는 반환값이다.
//     빈 ctrl.Result{}는 "재큐 없음"을, Requeue나 RequeueAfter를 채우면 "나중에 다시 불러 달라"를 뜻한다.
//   - 반환 타입 (ctrl.Result, error)처럼 Go는 값을 여러 개 돌려줄 수 있고, 관례상 마지막을 error로 둔다.
//     error를 nil이 아닌 값으로 돌려주면 컨트롤러 런타임이 지수 백오프로 자동 재시도한다.
//
// Reconcile과 그 Result에 대한 자세한 내용은 아래 참고
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *MLTrainingJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// logf.FromContext(ctx)는 컨트롤러 런타임이 ctx에 미리 심어 둔 로거를 꺼낸다.
	// 이 로거에는 재조정 대상 이름 같은 문맥이 이미 붙어 있어서, 로그를 나중에 요청 단위로 추적하기 좋다.
	log := logf.FromContext(ctx)
	// M1: 빈 reconciler, 요청만 log로 기록
	// batch/v1 Job 생성과 Kueue admission은 M5에서 다룸
	//
	// 설계 근거: M1의 목표는 CRD 스키마와 컨트롤러 배선이 실제로 동작하는지 확인하는 것이다.
	// 그래서 이 단계에서는 부수 효과 없이 요청을 관측만 하고, 실제 워크로드 생성은 뒤 마일스톤으로 미룬다.
	// 이렇게 해 두면 CRD round-trip과 컨트롤러 등록만 독립적으로 검증할 수 있다.
	//
	// Go 문법 설명: log.Info의 두 번째 인자부터는 "키, 값, 키, 값..." 쌍으로 이어지는 구조화 로그 필드다.
	log.Info("Reconciling MLTrainingJob", "name", req.Name, "namespace", req.Namespace)

	// 빈 Result와 nil error를 돌려준다.
	// "할 일을 다 했고 재큐도 필요 없다"는 뜻이며, 컨트롤러가 성공적으로 한 바퀴를 끝냈음을 알린다.
	return ctrl.Result{}, nil
}

// SetupWithManager: 이 controller를 Manager에 등록한다.
// cmd/main.go가 프로세스 시작 시 한 번 호출하며, 이 호출이 있어야 Reconcile이 이벤트를 받기 시작한다.
//
// Go 문법 설명:
//   - ctrl.Manager는 캐시, 클라이언트, 리더 선출 등 컨트롤러들이 공유하는 실행 환경이다.
//   - 아래는 "빌더 체인(builder chain)"이라는 패턴이다.
//     각 메서드가 빌더 자신을 다시 돌려주기 때문에 .For(...).Named(...).Complete(...)처럼 점으로 계속 이어 붙일 수 있다.
//   - 줄 끝의 점(.)은 "다음 줄에 이어진다"는 표시이며, Go의 자동 세미콜론 삽입을 피하려면 점을 줄 끝에 두어야 한다.
func (r *MLTrainingJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// ctrl.NewControllerManagedBy(mgr): 이 매니저가 수명을 관리하는 새 컨트롤러 빌더를 시작한다.
	return ctrl.NewControllerManagedBy(mgr).
		// For(...): 이 컨트롤러의 "주 대상(primary resource)"을 지정한다.
		// MLTrainingJob이 생성/수정/삭제될 때마다 그 이름이 작업 큐에 들어가 Reconcile이 불린다.
		// &platformv1.MLTrainingJob{}처럼 빈 값의 포인터를 넘기는 이유는 값이 아니라 "타입"을 알려주기 위해서다.
		For(&platformv1.MLTrainingJob{}).
		// Named(...): 컨트롤러 이름을 지정하며, 로그와 메트릭 레이블에 이 이름이 쓰인다.
		// 이름은 매니저 안에서 유일해야 하므로 중복되면 등록이 실패한다.
		Named("mltrainingjob").
		// Complete(r): 지금까지 설정한 내용으로 컨트롤러를 실제로 만들고, 리시버 r을 재조정기로 연결한다.
		// 체인의 마지막이며 error를 돌려주므로 그대로 호출한 쪽(main)에 올려보낸다.
		Complete(r)
}
