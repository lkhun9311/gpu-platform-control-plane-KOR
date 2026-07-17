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
// 같은 디렉터리(internal/controller)의 모든 .go 파일은 반드시 같은 package 이름(controller)을 가져야 한다.
//
// 그래서 이 파일이 정의한 상수와 헬퍼 함수들은 nodehealth_controller.go 같은 이웃 파일에서 import 없이 바로 쓸 수 있다.
//
// 설계 근거: 이 파일은 특정 컨트롤러의 Reconcile 본문이 아니라 컨트롤러들이 공유하는 "재료"만 모아 둔 곳이다.
//
// 상수와 작은 순수 함수를 여기로 빼면 같은 문자열을 여러 파일에 중복해 적는 실수를 막을 수 있다.
//
// 또한 이 헬퍼들은 쿠버네티스 API 서버를 호출하지 않고 메모리 안의 값만 다루므로 단위 테스트하기도 쉽다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
//
// 괄호로 묶으면 여러 개를 한 번에 나열할 수 있다.
//
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// slices: 슬라이스(배열 비슷한 것)를 다루는 표준 헬퍼 모음이며, 아래 slices.ContainsFunc에서 쓴다.
	"slices"

	// corev1: 쿠버네티스 코어 API 그룹의 v1 타입들이다(Node, Taint, NodeCondition 등).
	//
	// import 경로 앞의 corev1은 별칭(alias)이며, 원래 패키지 이름은 v1이라서 다른 v1들과 구분하려고 붙인다.
	corev1 "k8s.io/api/core/v1"
	// meta: status의 conditions 슬라이스를 표준 규칙대로 다루는 유틸리티이며, meta.SetStatusCondition을 쓴다.
	"k8s.io/apimachinery/pkg/api/meta"
	// metav1: 모든 쿠버네티스 리소스가 공유하는 메타 타입들이다(Condition, Time 등).
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(NodeHealth, NodeHealthStatus 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// nodeHealthFinalizer: NodeHealth 정리를 지키는 finalizer 문자열이다.
//
// 삭제 시 reconciler가 자신이 소유한 unhealthy taint를 먼저 걷어낸 뒤 이 finalizer를 제거한다.
//
// Go 문법 설명.
//
//   - const 는 상수 선언 키워드이며, 한 번 정해지면 실행 중에 바뀌지 않는 값이다.
//   - 소문자로 시작하는 이름은 이 패키지(controller) 안에서만 보이는 "비공개"다.
//     이 값은 컨트롤러 내부 구현 세부이므로 굳이 밖으로 공개하지 않는다.
//
// 설계 근거: finalizer는 "이 리소스를 실제로 지우기 전에 내가 할 정리가 있다"고 API 서버에 알리는 표식이다.
//
// finalizer가 붙어 있으면 삭제 요청이 와도 API 서버는 deletionTimestamp만 찍고 객체를 남겨 둔다.
//
// 그 사이에 reconciler가 node에서 unhealthy taint를 떼어내고, 정리가 끝나면 finalizer를 지워 실제 삭제가 진행되게 한다.
//
// 이게 없으면 NodeHealth만 사라지고 taint는 node에 영원히 남는 누수가 생긴다.
//
// 값이 도메인 이름 형식인 이유는 finalizer 이름이 "누구 소유인지" 충돌 없이 드러내야 한다는 쿠버네티스 관례 때문이다.
const nodeHealthFinalizer = "nodehealth.platform.lkhun9311.github.io/finalizer"

// unhealthyTaintKey/unhealthyTaintValue: not-ready node를 격리하려고 reconciler가 붙이는 taint의 key와 value다.
//
// scheduler가 해당 node에 GPU workload를 더 얹지 않도록 하며, reconciler는 오직 이 taint만 관리한다.
//
// Go 문법 설명.
//
//   - const ( ... ) 처럼 괄호로 묶으면 상수 여러 개를 한 블록에 모아 선언할 수 있다.
//   - 관련 있는 값끼리 묶어 두면 "이 둘은 한 세트"라는 의도가 코드에 드러난다.
//
// 설계 근거: taint는 node에 붙이는 "밀어내기 표식"이고, 이걸 견딜 toleration이 없는 pod은 그 node에 배치되지 않는다.
//
// 즉 node를 클러스터에서 빼지 않고도 새 workload 유입만 끊는 격리 수단이다.
//
// key를 우리 도메인 이름으로 시작하게 지어 다른 주체(예: cloud provider, GPU operator)가 붙인 taint와 절대 겹치지 않게 한다.
//
// 이 소유권 구분이 아래 isManagedTaint의 판정 근거가 된다.
const (
	unhealthyTaintKey   = "platform.lkhun9311.github.io/unhealthy"
	unhealthyTaintValue = "true"
)

// faultSourceNodeNotReady: node가 not-ready라 격리되는 동안 기록되는 faultSignal의 source 값이다.
//
// 설계 근거: 이는 readiness에서 파생된 신호이지 실제 hardware 결함 신호는 아니다.
//
// 그래서 source 이름을 "gpu-fault" 같은 과장된 말 대신 관찰한 사실 그대로인 "node-not-ready"로 정직하게 적는다.
//
// 나중에 진짜 hardware 신호(예: XID 에러)가 들어오면 다른 source 값으로 구분해 넣을 수 있게 여지를 남기는 뜻도 있다.
const faultSourceNodeNotReady = "node-not-ready"

// conditionReady: 대상 node의 readiness를 그대로 반영하는 NodeHealth condition의 type 이름이다.
//
// 설계 근거: condition은 쿠버네티스에서 "관찰된 상태 한 조각"을 표준 형식으로 표현하는 방법이다.
//
// type(무엇에 대한 이야기인지), status(True/False/Unknown), reason(기계가 읽는 짧은 이유), message(사람이 읽는 설명)로 구성된다.
//
// phase가 "지금 한 단어로 요약하면?"이라면 condition은 "왜 그렇게 판단했는가?"를 남기는 자리다.
const conditionReady = "Ready"

// conditionReady용 condition reason들이다.
//
// reason은 사람이 읽는 문장이 아니라 기계가 비교하는 짧은 식별자이므로 CamelCase 한 단어로 짓는 게 관례다.
//
// 설계 근거: 세 값은 reconciler가 구분하는 세 가지 상황에 각각 대응한다.
//
//   - reasonNodeReady: 대상 node를 찾았고 그 node의 Ready condition이 True다.
//   - reasonNodeNotReady: 대상 node를 찾았지만 Ready가 True가 아니다.
//   - reasonNodeNotFound: 대상 node 자체가 아직(또는 더 이상) 클러스터에 없다.
//
// "없음"과 "있는데 안 좋음"을 다른 reason으로 나눠야 운영자가 알림을 보고 원인을 바로 가를 수 있다.
const (
	reasonNodeReady    = "NodeReady"
	reasonNodeNotReady = "NodeNotReady"
	reasonNodeNotFound = "NodeNotFound"
)

// M3에서 방출되는 NodeHealth phase들이다.
//
// M3는 readiness를 Pending(node 없음), Ready(node 준비됨), Quarantine(node not-ready라 taint 부착)으로 몰아간다.
//
// 설계 근거: CRD enum의 Intake, Degraded phase는 이후 lifecycle 단계용 예약(docs/03 참고)이라 여기서는 방출 안 한다.
//
// CRD 스키마에는 미래의 값까지 미리 적어 두되 컨트롤러는 지금 실제로 구현한 값만 쓰는 방식이다.
//
// 이렇게 하면 나중에 단계를 추가할 때 CRD를 다시 바꾸지 않아도 되고, 지금 시점에는 "구현하지 않은 상태를 status에 적어 거짓말하는" 일을 피할 수 있다.
const (
	phasePending    = "Pending"
	phaseReady      = "Ready"
	phaseQuarantine = "Quarantine"
)

// setPhase: phase가 실제로 바뀔 때만 phase를 갱신하고 lastTransitionTime을 올린다.
//
// Go 문법 설명.
//
//   - 리시버가 없는 일반 함수다(특정 타입에 붙는 메서드가 아니다).
//   - 첫 인자 status가 *platformv1.NodeHealthStatus 처럼 별표(*)가 붙은 포인터인 점이 핵심이다.
//     포인터로 받아야 이 함수 안에서 한 수정이 호출한 쪽의 원본 status에 그대로 반영된다.
//     값으로 받으면 복사본만 고치고 끝나므로 아무 일도 일어나지 않는다.
//   - 반환값이 없다(함수 시그니처 끝에 타입이 없다).
//     결과를 돌려주는 대신 인자로 받은 status를 직접 고치는 방식이라 그렇다.
//
// 설계 근거: lastTransitionTime은 이름 그대로 "마지막으로 전이(transition)한 시각"이다.
//
// 같은 phase를 다시 써 넣을 때마다 시각을 갱신해 버리면 이 필드는 "마지막으로 reconcile이 돈 시각"이 되어 의미를 잃는다.
//
// reconcile은 아무 변화가 없어도 주기적으로 다시 도는 것이 정상이므로 이 구분이 특히 중요하다.
//
// 덤으로, 값이 그대로면 status 쓰기 자체가 생략되어 API 서버에 불필요한 write와 watch 이벤트를 만들지 않는다.
func setPhase(status *platformv1.NodeHealthStatus, phase string) {
	// 이미 같은 phase면 아무것도 하지 않고 즉시 빠져나온다.
	//
	// == 는 "같다" 비교 연산자이고, 인자 없는 return은 "여기서 함수를 끝낸다"는 뜻이다.
	if status.Phase == phase { // 동일 phase면 시각 갱신 없이 조기 반환
		return
	}
	// 여기까지 왔다는 건 phase가 실제로 달라졌다는 뜻이므로 새 값을 써 넣는다.
	status.Phase = phase
	// metav1.Now()는 현재 시각을 쿠버네티스 API가 쓰는 metav1.Time 타입으로 만들어 준다.
	//
	// := 는 변수를 선언하면서 동시에 값을 넣는 축약 문법이며, 타입은 오른쪽 값에서 자동으로 추론된다.
	now := metav1.Now()
	// LastTransitionTime 필드는 *metav1.Time(포인터) 타입이라 &now 로 now의 주소를 넣는다.
	//
	// 포인터인 이유는 "아직 한 번도 전이한 적 없음"을 nil로 표현할 수 있어야 하기 때문이다.
	//
	// 지역 변수 now를 먼저 만든 것도 &metav1.Now() 처럼 함수 반환값에 바로 &를 붙일 수는 없어서다.
	status.LastTransitionTime = &now
}

// setReadyCondition: Ready condition을 설정하며 observedGeneration을 찍는다.
//
// meta.SetStatusCondition을 감싼 얇은 wrapper라 값이 안 바뀌면 lastTransitionTime을 보존한다.
//
// Go 문법 설명.
//
//   - (status *platformv1.NodeHealthStatus, ready bool, reason, msg string, generation int64) 가 인자 목록이다.
//     reason, msg string 처럼 타입이 같고 연달아 오는 인자는 타입을 마지막에 한 번만 적어도 된다.
//   - bool은 참/거짓 값이고, int64는 64비트 정수 타입이다.
//   - meta.SetStatusCondition(&status.Conditions, ...) 에서 & 는 슬라이스의 주소를 넘긴다는 뜻이다.
//     이 함수가 슬라이스에 항목을 추가하거나 교체하려면 원본을 가리켜야 하므로 포인터가 필요하다.
//
// 설계 근거: conditions는 단순한 배열이 아니라 "type이 유일한 키" 역할을 하는 목록이다.
//
// 그래서 그냥 append하면 Ready condition이 여러 개 쌓이는 잘못된 status가 만들어진다.
//
// meta.SetStatusCondition은 같은 type이 있으면 교체하고 없으면 추가하는 upsert를 대신 해 주고, status 값이 이전과 같으면 lastTransitionTime을 그대로 두는 규칙까지 표준대로 지켜 준다.
//
// 이 wrapper를 두는 이유는 호출부마다 metav1.Condition 리터럴을 손으로 채우다 Type이나 ObservedGeneration을 빠뜨리는 걸 막기 위해서다.
func setReadyCondition(status *platformv1.NodeHealthStatus, ready bool, reason, msg string, generation int64) {
	// 호출부는 다루기 쉬운 bool로 주지만, condition의 Status 필드는 문자열 기반의 3상태 타입이다.
	//
	// (True / False / Unknown 세 가지를 표현할 수 있어야 해서 bool이 아니다.)
	//
	// 그래서 일단 False로 두고 아래에서 필요할 때만 True로 바꾸는 방식으로 변환한다.
	condStatus := metav1.ConditionFalse
	if ready {
		condStatus = metav1.ConditionTrue
	}
	// metav1.Condition{...} 는 구조체 값을 만들면서 필드 이름을 직접 지정해 채우는 문법이다.
	//
	// 필드명을 적으면 순서를 외울 필요가 없고, 나중에 타입에 필드가 추가돼도 이 코드가 깨지지 않는다.
	//
	// 각 필드의 뜻은 다음과 같다.
	//
	//   - Type: 이 condition이 무엇에 대한 이야기인지를 나타내는 유일한 키다.
	//   - Status: True/False/Unknown 중 하나로 판정 결과를 담는다.
	//   - Reason: 기계가 비교하는 짧은 이유 식별자다.
	//   - Message: 사람이 읽는 자연어 설명이다.
	//   - ObservedGeneration: "이 판단은 spec의 몇 번째 버전을 보고 내린 것인가"를 기록한다.
	//
	// generation은 spec이 바뀔 때마다 API 서버가 1씩 올려 주는 번호다.
	//
	// ObservedGeneration이 현재 generation보다 작으면 status가 아직 최신 spec을 반영하지 못한 것이므로, 운영자와 도구가 "낡은 status를 보고 판단하는" 실수를 피할 수 있다.
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
}

// isNodeReady: node의 Ready condition이 True인지 여부를 반환한다.
//
// Go 문법 설명.
//
//   - 반환 타입 bool 은 참/거짓 값이다.
//   - for i := range node.Status.Conditions 는 슬라이스를 처음부터 끝까지 훑는 반복문이며, range가 인덱스 i를 0,1,2...로 준다.
//
// 설계 근거: 쿠버네티스 node는 자신의 상태를 phase 하나가 아니라 conditions 목록으로 알린다.
//
// 그중 우리가 필요한 건 Ready 하나뿐인데, 그 항목이 목록의 몇 번째에 있는지는 보장되지 않는다.
//
// 그래서 매번 찾아 훑는 이 판정 로직을 헬퍼로 한 번만 작성해 두고 컨트롤러는 이름만 부른다.
//
// Ready condition이 아예 없거나 Unknown이면 false를 돌려준다.
//
// "확실히 Ready인 경우에만 Ready로 인정"하는 보수적인 규칙이며, 상태를 모를 때 격리 쪽으로 기우는 편이 GPU workload에는 안전하다.
func isNodeReady(node *corev1.Node) bool {
	// 인덱스로 도는 이유는 요소가 구조체라 값 복사를 한 번만 하도록 다루기 위해서다.
	for i := range node.Status.Conditions {
		// c: i번째 condition의 복사본이며, 읽기만 할 것이므로 복사본으로 충분하다.
		c := node.Status.Conditions[i]
		// corev1.NodeReady는 "Ready"라는 condition type을 가리키는 상수다.
		//
		// 문자열 "Ready"를 직접 적지 않고 상수를 쓰면 오타가 컴파일 단계에서 잡힌다.
		if c.Type == corev1.NodeReady {
			// 찾던 항목을 만났으니 그 status가 True인지 비교한 결과를 바로 돌려주고 끝낸다.
			//
			// 목록에 Ready는 하나뿐이므로 더 훑을 이유가 없다.
			return c.Status == corev1.ConditionTrue
		}
	}
	// 반복이 끝났다는 건 Ready condition을 하나도 못 찾았다는 뜻이다.
	//
	// 아직 kubelet이 보고를 시작하지 않은 새 node 등에서 생길 수 있으며, 이때는 "Ready 아님"으로 본다.
	return false
}

// isManagedTaint: 이 controller가 관리하는 바로 그 taint인지 여부를 반환한다.
//
// key와 effect 둘 다로 식별하므로 key는 같아도 effect가 다른(다른 주체 소유) taint는 건드리지 않는다.
//
// Go 문법 설명.
//
//   - 인자 t corev1.Taint 는 포인터가 아닌 값으로 받는다.
//     Taint는 문자열 몇 개로 된 작은 구조체이고 이 함수는 읽기만 하므로 복사본으로 충분하다.
//   - && 는 "그리고(AND)"이며, 앞 조건이 거짓이면 뒤는 검사하지 않는다(short-circuit).
//   - 이 시그니처 func(corev1.Taint) bool 는 아래 slices.ContainsFunc가 요구하는 술어(predicate) 형태와 정확히 맞다.
//     그래서 함수 이름 자체를 값처럼 넘길 수 있다.
//
// 설계 근거: 이 함수 하나가 taint 소유권 판정의 유일한 기준점이다.
//
// 붙일 때(ensureUnhealthyTaint)와 뗄 때(removeUnhealthyTaint)가 같은 판정을 쓰도록 강제해 둔 것이다.
//
// 만약 붙일 때와 뗄 때의 조건이 조금이라도 어긋나면 taint가 중복으로 쌓이거나 영영 안 지워지는 버그가 난다.
//
// key만으로 판정하지 않는 이유는 같은 key에 NoExecute 같은 다른 effect를 붙인 주체가 있을 수 있어서다.
//
// 남의 taint를 지우면 그쪽 컨트롤러와 서로 쓰고 지우는 싸움이 벌어지므로, 우리는 우리가 붙인 (key, effect) 조합만 손댄다.
func isManagedTaint(t corev1.Taint) bool {
	return t.Key == unhealthyTaintKey && t.Effect == corev1.TaintEffectNoSchedule
}

// ensureUnhealthyTaint: unhealthy taint가 없으면 붙인다.
//
// node의 taint가 바뀌었는지 반환하며, 다른 taint는 그대로 둔다.
//
// Go 문법 설명.
//
//   - 인자 node가 포인터(*corev1.Node)라서 이 함수가 node.Spec.Taints를 고치면 호출한 쪽의 원본이 바뀐다.
//   - 반환값 bool 은 "내가 실제로 뭔가 바꿨는가"를 알린다.
//
// 설계 근거: 이름의 "ensure"는 쿠버네티스 컨트롤러에서 "원하는 상태로 맞춘다"는 뜻으로 쓰는 관례적 접두사다.
//
// "붙여라"가 아니라 "붙어 있게 하라"이므로 몇 번을 호출해도 결과가 같다(idempotent).
//
// reconcile은 같은 상태에서 몇 번이고 다시 도는 것이 정상이라 이 성질이 필수다.
//
// 변경 여부를 bool로 돌려주는 이유는 호출부가 진짜 바뀐 경우에만 node를 Update하도록 하기 위해서다.
//
// 안 바뀌었는데 Update를 보내면 불필요한 API 서버 write가 발생하고, 그 write가 다시 watch 이벤트를 일으켜 reconcile이 자기 자신을 계속 깨우는 무한 루프가 된다.
func ensureUnhealthyTaint(node *corev1.Node) bool {
	// slices.ContainsFunc(슬라이스, 판정함수)는 조건을 만족하는 요소가 하나라도 있으면 true를 준다.
	//
	// isManagedTaint 뒤에 괄호를 붙이지 않은 점에 주목하자.
	//
	// 호출 결과가 아니라 함수 그 자체를 값으로 넘기는 것이며, ContainsFunc가 요소마다 대신 불러 준다.
	if slices.ContainsFunc(node.Spec.Taints, isManagedTaint) { // 이미 있으면 그대로 둠
		// 이미 원하는 상태이므로 아무것도 바꾸지 않았다는 뜻으로 false를 돌려준다.
		return false
	}
	// 없으니 새로 만들어 목록 끝에 덧붙인다.
	//
	// append(슬라이스, 새요소)는 요소를 추가한 새 슬라이스를 돌려주므로 반드시 원래 자리에 다시 대입해야 한다.
	//
	// 기존 taint들을 지우고 새로 쓰는 게 아니라 뒤에 더하는 것이므로 남의 taint는 전부 보존된다.
	//
	// Effect를 NoSchedule로 두는 것은 의도적인 선택이다.
	//
	// NoSchedule은 이 node에 새 pod이 배치되는 것만 막고 이미 돌고 있는 pod은 쫓아내지 않는다.
	//
	// NoExecute를 쓰면 실행 중인 pod까지 즉시 퇴출되므로, 일시적 not-ready에 진행 중인 훈련 job을 날려 버릴 수 있어 쓰지 않는다.
	//
	// 여기서 쓰는 Effect 값은 위 isManagedTaint의 판정 조건과 반드시 일치해야 한다.
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    unhealthyTaintKey,
		Value:  unhealthyTaintValue,
		Effect: corev1.TaintEffectNoSchedule,
	})
	// 실제로 taint를 추가했으므로 호출부에 "이제 node를 Update해야 한다"고 알린다.
	return true
}

// removeUnhealthyTaint: 이 controller가 관리하는 taint만 있을 때 제거한다.
//
// node의 taint가 바뀌었는지 반환하며, key는 같아도 effect가 다른 taint를 포함해 나머지 taint는 보존한다.
//
// Go 문법 설명.
//
//   - ensureUnhealthyTaint와 시그니처가 대칭이다(포인터 node를 받고 변경 여부 bool을 돌려준다).
//     짝이 되는 두 동작을 같은 모양으로 맞춰 두면 호출부의 사용법도 똑같아진다.
//
// 설계 근거: 이 함수는 node가 다시 Ready가 되었을 때와 NodeHealth가 삭제될 때(finalizer 정리) 양쪽에서 쓰인다.
//
// 격리를 걸 줄만 알고 풀 줄 모르면 한 번 not-ready였던 node가 영원히 스케줄 불가로 남는다.
//
// 구현이 "지울 것을 찾아 삭제"가 아니라 "남길 것만 모아 새로 만들기"인 이유는 이 편이 훨씬 안전해서다.
//
// 슬라이스를 순회하면서 그 자리에서 요소를 지우면 인덱스가 밀려 항목을 건너뛰는 고전적인 버그가 나기 쉽다.
func removeUnhealthyTaint(node *corev1.Node) bool {
	// kept: 살려 둘 taint들을 모을 새 슬라이스이며, 아직 아무것도 없으니 nil 상태다.
	//
	// var 로만 선언한 nil 슬라이스에도 append는 정상 동작하므로 make로 미리 만들 필요가 없다.
	//
	// (map과 달리 슬라이스는 nil이어도 append가 알아서 새 배열을 잡아 준다.)
	var kept []corev1.Taint
	// changed: 관리 대상 taint를 하나라도 걸러 냈는지 기록하는 깃발이며, 처음엔 false다.
	changed := false
	for i := range node.Spec.Taints {
		// 우리가 관리하는 taint라면 kept에 넣지 않고 넘어간다(= 결과적으로 삭제된다).
		//
		// continue는 이번 반복을 여기서 끝내고 다음 요소로 넘어가라는 뜻이다.
		if isManagedTaint(node.Spec.Taints[i]) { // 관리 대상만 걸러 버림
			changed = true
			continue
		}
		// 우리 것이 아니면 그대로 kept에 옮겨 담아 보존한다.
		kept = append(kept, node.Spec.Taints[i])
	}
	// 아무것도 안 걸러 냈다면 원본을 건드리지 않고 그대로 둔다.
	//
	// 이 if가 없으면 taint가 0개인 node의 필드를 빈 슬라이스에서 nil로 바꿔 의미 없는 diff를 만들 수 있다.
	if changed {
		node.Spec.Taints = kept
	}
	// 호출부는 이 값이 true일 때만 node를 Update한다.
	return changed
}
