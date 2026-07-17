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
// 같은 디렉터리(api/v1)의 모든 .go 파일은 반드시 같은 package 이름(v1)을 가져야 한다.
// 그래서 groupversion_info.go가 만든 SchemeBuilder를 이 파일에서 import 없이 바로 쓸 수 있다.
package v1

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
import (
	// metav1: 쿠버네티스의 모든 리소스가 공통으로 쓰는 메타데이터 타입 모음이다.
	// TypeMeta, ObjectMeta, Time, Condition 등이 여기 들어 있다.
	// import 경로 앞의 metav1은 별칭(alias)이며, 원래 패키지 이름 v1이 이 패키지 이름(v1)과 겹쳐서 붙인 것이다.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 이 파일은 직접 고쳐 가며 채워 넣는 기본 골격이다.
// 참고: json tag는 필수이고, 새 field를 추가할 때 직렬화되려면 반드시 json tag를 붙여야 한다.
//
// Go 문법 설명(struct tag):
//   - 필드 선언 뒤 백틱(`)으로 감싼 문자열을 struct tag라고 부른다.
//   - `json:"queue"` 는 이 필드를 JSON/YAML로 바꿀 때 queue라는 키를 쓰라는 지시다.
//   - tag를 빼면 Go 필드 이름(대문자로 시작)이 그대로 키가 되어 쿠버네티스 관례(camelCase)를 어긴다.
//   - tag는 주석이 아니라 코드이며, 리플렉션으로 런타임에 읽히므로 오타가 나면 직렬화가 조용히 깨진다.
//
// Go 문법 설명(+kubebuilder 마커):
//   - // +kubebuilder:... 나 // +optional 처럼 생긴 줄은 주석처럼 보이지만 사실상 코드다.
//   - controller-gen 도구가 이 줄들을 읽어서 CRD YAML의 검증 규칙과 스키마를 만들어 낸다.
//   - 따라서 번역하거나 지우면 생성되는 CRD가 달라지므로 원문 그대로 두어야 한다.

// MLTrainingJobSpec: MLTrainingJob의 원하는 상태(desired state) 정의다.
//
// 설계 근거: 사용자는 여기에 "무엇을 어떤 큐에서 얼마나 병렬로 학습시킬지"만 적는다.
// 컨트롤러가 이 spec을 읽어 실제 batch/v1 Job을 만들어 desired state에 수렴시킨다.
type MLTrainingJobSpec struct {
	// 이 job이 admission을 거치는 Kueue LocalQueue 이름이다(같은 namespace).
	//
	// Go 문법 설명: string은 Go의 문자열 타입이고 기본값(zero value)은 빈 문자열("")이다.
	// 아래 +required 마커가 있으므로 API 서버가 이 필드를 반드시 요구한다.
	//
	// 설계 근거: LocalQueue는 namespace 스코프 객체이므로 큐 이름만 있으면 되고 namespace를 따로 받지 않는다.
	// 이 MLTrainingJob과 같은 namespace의 큐를 찾는다.
	//
	// +required
	Queue string `json:"queue"`

	// 학습 container image다.
	//
	// +required
	Image string `json:"image"`

	// container entrypoint 재정의다.
	//
	// Go 문법 설명(슬라이스):
	//   - []string 은 "문자열들이 순서대로 담긴 슬라이스"이며 가변 길이 목록이다.
	//   - zero value는 nil이고, nil 슬라이스는 len()이나 range 반복에서 빈 목록처럼 안전하게 동작한다.
	//   - 그래서 선택 필드인 슬라이스는 포인터로 감쌀 필요 없이 nil 자체가 "설정 안 함"을 뜻한다.
	//   - omitempty는 길이가 0이면 JSON 키를 아예 만들지 말라는 옵션이다.
	//
	// 설계 근거: 비어 있으면 컨테이너 이미지에 내장된 기본 entrypoint를 그대로 쓴다.
	//
	// +optional
	Command []string `json:"command,omitempty"`

	// 예시용 GPU class다(예: "l40s").
	// local에서는 시뮬레이션된 용량을 기준으로 동작한다(개발 runbook 참고).
	//
	// Go 문법 설명: 선택 필드지만 포인터가 아니라 값 타입 string이다.
	// 문자열은 "비어 있음"을 빈 문자열로 표현할 수 있어서 nil 포인터가 필요 없기 때문이다.
	//
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`

	// pod당 GPU 수다(nvidia.com/gpu).
	// local에서는 실제 hardware가 아니라 시뮬레이션된 용량을 기준으로 한다.
	//
	// Go 문법 설명: int32는 32비트 정수 타입이다.
	// 쿠버네티스 API는 플랫폼마다 크기가 달라지는 int 대신 크기가 고정된 int32/int64를 쓰는 것이 관례다.
	// Minimum=0 마커 때문에 음수는 API 서버가 거부하며, 0은 "GPU 없는 CPU 전용 학습"을 뜻하는 유효한 값이다.
	//
	// 설계 근거: 이 값은 pod 하나당 수치이므로 총 GPU 사용량은 gpuCount 곱하기 parallelism이 된다.
	// Kueue가 이 총량으로 quota를 판단해 admission을 결정한다.
	//
	// +kubebuilder:validation:Minimum=0
	// +required
	GPUCount int32 `json:"gpuCount"`

	// batch/v1 Job의 parallelism이다(동시 실행 pod 수).
	//
	// Go 문법 설명(+kubebuilder:default과 +optional의 관계):
	//   - +kubebuilder:default=1 은 사용자가 값을 생략하면 API 서버가 1을 채워 넣게 한다.
	//   - 기본값이 있으므로 이 필드는 포인터가 아니어도 된다.
	//     사용자가 생략해도 int32의 zero value인 0이 남는 게 아니라 API 서버가 1로 덮어써 주기 때문이다.
	//   - 만약 default 마커가 없었다면 "생략"과 "0으로 명시"가 구분되지 않아 포인터가 필요했을 것이다.
	//
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Parallelism int32 `json:"parallelism,omitempty"`

	// batch/v1 Job의 completions다(성공해야 하는 pod 수).
	//
	// 설계 근거: parallelism은 "한 번에 몇 개를 띄울지", completions는 "총 몇 개가 성공해야 job이 끝나는지"다.
	// 둘 다 기본값 1이라 아무것도 적지 않으면 pod 하나짜리 단순 학습 job이 된다.
	//
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Completions int32 `json:"completions,omitempty"`
}

// MLTrainingJobStatus: MLTrainingJob의 관찰된 상태(observed state) 정의다.
//
// 설계 근거: status는 컨트롤러만 쓰는 영역이다.
// 아래 MLTrainingJob 타입에 붙은 +kubebuilder:subresource:status 마커 덕분에 status는 별도 엔드포인트로 분리된다.
// 그래서 사용자가 spec을 수정해도 status가 덮어써지지 않고, 컨트롤러가 status를 써도 generation이 오르지 않는다.
type MLTrainingJobStatus struct {
	// Kueue admission과 실행 수명주기를 추적한다.
	//
	// Go 문법 설명: Go에는 enum 문법이 없어서 그냥 string으로 두고 아래 Enum 마커로 값을 제한한다.
	// 마커에 적힌 Pending, Admitted, Running, Succeeded, Failed 외의 값은 API 서버가 거부한다.
	//
	// 설계 근거: Pending은 아직 큐에서 대기 중, Admitted는 Kueue가 quota를 확인해 통과시킨 상태다.
	// 즉 admission 단계와 실행 단계를 하나의 phase 축에 순서대로 늘어놓은 것이다.
	//
	// +kubebuilder:validation:Enum=Pending;Admitted;Running;Succeeded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// controller가 마지막으로 관찰한 generation이다.
	//
	// Go 문법 설명: metadata.generation은 int64라서 여기서도 int64로 맞춘다.
	//
	// 설계 근거: generation은 spec이 바뀔 때마다 API 서버가 1씩 올려 주는 번호다.
	// observedGeneration이 metadata.generation보다 작으면 "아직 최신 spec을 반영하지 못했다"는 뜻이다.
	// 이 값으로 status가 최신인지 여부를 판단할 수 있다.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase가 마지막으로 바뀐 시각이다.
	//
	// Go 문법 설명: metav1.Time은 표준 time.Time을 RFC3339 문자열로 직렬화하도록 감싼 쿠버네티스 타입이다.
	// 포인터(*metav1.Time)인 이유는 "아직 한 번도 전이가 없었음"을 nil로 표현하기 위해서다.
	// 값 타입이면 생략했을 때 0001-01-01 같은 무의미한 시각이 들어가 버린다.
	//
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// MLTrainingJob resource의 현재 상태 표현이다.
	// 각 condition의 status는 True, False, Unknown 중 하나다.
	//
	// Go 문법 설명(슬라이스): []metav1.Condition 은 Condition 값들이 순서대로 담긴 가변 길이 목록이다.
	// 슬라이스의 zero value는 nil이며, nil 슬라이스도 range 반복에서 빈 목록처럼 안전하게 동작한다.
	//
	// Go 문법 설명(마커): +listType=map 과 +listMapKey=type 은 이 목록을 "type 필드를 키로 하는 map처럼" 다루라는 지시다.
	// 이렇게 해야 서버 사이드 적용(server-side apply)에서 여러 컨트롤러가 서로 다른 condition을 충돌 없이 병합할 수 있다.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// 아래 마커들은 controller-gen이 CRD를 만들 때 읽는 지시문이다.
//   - object:root=true 는 이 타입이 최상위 API 리소스임을 알린다(단순 중첩 구조체와 구분).
//   - subresource:status 는 /status 하위 리소스를 따로 만들어 spec과 status의 쓰기 권한을 분리한다.
//   - printcolumn 들은 kubectl get 출력에 보일 열을 정의하며, JSONPath로 어떤 필드를 뽑을지 지정한다.
//   - scope 마커가 없으므로 이 리소스는 기본값인 Namespaced다.
//     학습 job은 팀의 namespace 안에서 생성되고, spec.queue가 가리키는 LocalQueue도 같은 namespace에 있기 때문이다.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Queue",type=string,JSONPath=`.spec.queue`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MLTrainingJob: mltrainingjobs API의 schema다.
// 사용자가 YAML로 작성하고 kubectl apply로 클러스터에 올리는 객체 하나에 대응한다.
type MLTrainingJob struct {
	// Go 문법 설명(임베딩): 필드 이름 없이 타입만 적으면 "임베딩(embedding)"이다.
	// 임베딩된 타입의 필드와 메서드는 바깥 타입의 것처럼 승격되어 job.Kind 처럼 바로 접근할 수 있다.
	// TypeMeta는 apiVersion과 kind 두 필드를 갖는다.
	// json 태그의 inline은 중첩 object를 만들지 말고 두 필드를 이 객체의 최상위에 펼치라는 뜻이다.
	// 그래서 YAML에서 apiVersion과 kind가 metadata와 같은 높이에 나온다.
	metav1.TypeMeta `json:",inline"`

	// 표준 object metadata다.
	// name, namespace, labels, annotations, creationTimestamp, generation 등이 여기 들어 있다.
	//
	// Go 문법 설명: ObjectMeta도 임베딩이지만 태그가 `json:"metadata,omitzero"`라서 metadata라는 키 아래에 중첩된다.
	// omitzero는 값이 zero value면 키를 통째로 생략하라는 옵션이다.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// MLTrainingJob의 원하는 상태 정의다.
	//
	// Go 문법 설명: 이쪽은 이름 있는 필드라서 job.Spec.Queue 처럼 접근한다.
	//
	// +required
	Spec MLTrainingJobSpec `json:"spec"`

	// MLTrainingJob의 관찰된 상태 정의다.
	//
	// +optional
	Status MLTrainingJobStatus `json:"status,omitzero"`
}

// 이 목록 타입도 최상위 API 객체이므로 root 마커가 필요하다.
// 다만 목록은 status를 갖지 않으므로 subresource 마커는 붙이지 않는다.
//
// +kubebuilder:object:root=true

// MLTrainingJobList: MLTrainingJob 목록을 포함하는 컨테이너 타입이다.
//
// 설계 근거: 쿠버네티스 클라이언트의 List 호출은 결과를 이런 List 타입에 채워 준다.
// 컨트롤러가 &list를 넘겨 받아 list.Items를 훑는 것이 바로 이 타입이다.
//
// 각 필드 설명:
//   - TypeMeta: apiVersion과 kind를 담으며, 목록의 kind는 "MLTrainingJobList"가 된다.
//   - ListMeta: 개별 객체의 ObjectMeta와 달리 목록 전체의 메타데이터다.
//     resourceVersion이나 페이지네이션용 continue 토큰 등이 들어 있다.
//   - Items: 실제 MLTrainingJob 객체들이 담기는 슬라이스다.
//     포인터가 아니라 값의 슬라이스이므로 원소의 주소가 필요하면 &list.Items[i] 처럼 인덱스로 접근해야 한다.
//
// Go 문법 설명: 세 필드가 빈 줄 없이 붙어 있으므로 gofmt가 이들의 struct tag 시작 열을 하나로 맞춰 정렬한다.
// 그래서 Items 뒤에 공백이 여러 칸 들어가 있는 것이며, 이 정렬은 gofmt가 관리하므로 손으로 건드리지 않는다.
type MLTrainingJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MLTrainingJob `json:"items"`
}

// init: 이 패키지가 처음 로드될 때 Go 런타임이 자동으로 딱 한 번 호출하는 특수 함수다.
//
// Go 문법 설명:
//   - init은 인자도 반환값도 없어야 하며, 직접 호출할 수 없다.
//   - 한 패키지에 여러 개 있어도 되고 전부 실행된다.
//     이 패키지의 CRD 파일마다 하나씩 두고 각자 자기 타입만 등록하는 이유가 그것이다.
//
// 설계 근거: 여기서 두 타입을 scheme에 등록해 두면, 클라이언트가 "platform.lkhun9311.github.io/v1, Kind=MLTrainingJob"이라는
// 그룹/버전/종류 문자열과 이 Go 타입을 서로 변환할 수 있게 된다.
// &MLTrainingJob{} 는 빈 값을 만들어 그 포인터를 넘기는 표현이며, 등록에는 타입 정보만 필요해서 내용은 비워 둔다.
func init() {
	SchemeBuilder.Register(&MLTrainingJob{}, &MLTrainingJobList{})
}
