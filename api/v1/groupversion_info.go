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

// Package v1은 platform v1 API group의 API schema 정의 포함
//
// Go 문법 설명:
//   - package 선언 바로 위에 붙은 주석은 "패키지 주석(package comment)"이라고 부른다.
//     이 주석은 go doc 같은 문서 도구가 패키지 설명으로 그대로 가져다 쓴다.
//     관례상 "Package <이름>은 ..." 형태로 시작한다.
//   - 같은 디렉터리(api/v1)의 모든 .go 파일은 package v1이어야 한다.
//     그래서 gpuquotapolicy_types.go 등에 정의된 CRD 타입들을 이 파일에서 import 없이 참조할 수 있다.
//
// 아래 두 줄은 주석처럼 생겼지만 실제로는 controller-gen이 읽는 "마커(marker)"라는 코드다.
// 컴파일러는 무시하지만 코드 생성기는 이 줄을 읽고 동작을 바꾸므로 절대 번역하거나 지우면 안 된다.
//   - +kubebuilder:object:generate=true 는 이 패키지의 타입들에 대해
//     DeepCopy 메서드(zz_generated.deepcopy.go)를 자동 생성하라는 지시다.
//   - +groupName=... 은 이 패키지의 타입들이 속할 API group 이름을 CRD 매니페스트 생성기에 알려준다.
//
// +kubebuilder:object:generate=true
// +groupName=platform.lkhun9311.github.io
package v1

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 괄호로 묶으면 여러 개를 한 번에 나열할 수 있다.
import (
	// schema: GroupVersion, GroupVersionKind 같은 "API 좌표" 타입들을 담은 쿠버네티스 표준 패키지다.
	// 쿠버네티스는 모든 리소스를 (group, version, kind) 세 값으로 식별한다.
	"k8s.io/apimachinery/pkg/runtime/schema"
	// scheme: controller-runtime이 제공하는 scheme 등록 헬퍼(Builder)를 담은 패키지다.
	// 순수 client-go로 scheme을 등록하려면 보일러플레이트가 길어지는데 Builder가 그걸 줄여준다.
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// var ( ... ) 블록: 여러 개의 패키지 전역 변수를 한 번에 선언하는 문법이다.
// 개별로 var를 반복해 쓰는 것과 의미는 같고 묶어서 읽기 좋게 만든 것뿐이다.
// 여기 선언된 네 값은 모두 대문자로 시작하므로 패키지 밖(cmd/main.go 등)에서도 보이는 "공개(export)" 심볼이다.
//
// 설계 근거(설계서 Components 절): 이 파일은 CRD 타입들이 어떤 API group/version에 속하는지를 한곳에서 정의한다.
// controller-manager와 게이트웨이 두 바이너리 모두 여기의 AddToScheme을 호출해 같은 타입 등록 정보를 공유한다.
// 등록 정보가 한 군데에만 있어야 두 바이너리가 같은 CRD를 같은 방식으로 해석한다.
var (
	// SchemeGroupVersion: 이 패키지의 타입들이 속한 (group, version) 쌍이다.
	// group은 "platform.lkhun9311.github.io", version은 "v1"이다.
	// 쿠버네티스에서 이 값은 매니페스트의 apiVersion: platform.lkhun9311.github.io/v1 에 대응한다.
	//
	// Go 문법 설명:
	//   - schema.GroupVersion{Group: ..., Version: ...} 는 "구조체 리터럴"이다.
	//     타입 이름 뒤에 중괄호를 쓰고 필드이름: 값 형태로 초기값을 채워 값 하나를 만든다.
	//   - 필드 이름을 명시했으므로 필드 순서에 의존하지 않아 나중에 구조체가 바뀌어도 깨지지 않는다.
	//
	// 이 object들을 등록하는 데 쓰이는 group version,
	// applyconfiguration 생성기(예: controller-gen)가 이 이름을 사용한다.
	SchemeGroupVersion = schema.GroupVersion{Group: "platform.lkhun9311.github.io", Version: "v1"}

	// GroupVersion: 위 SchemeGroupVersion과 완전히 같은 값을 가리키는 두 번째 이름이다.
	// kubebuilder가 만든 코드는 GroupVersion을, client-go 계열 생성기는 SchemeGroupVersion을 기대해서 둘 다 둔다.
	//
	// Go 문법 설명:
	//   - 우변에 다른 변수를 그대로 쓰면 그 시점의 값이 복사된다.
	//   - GroupVersion은 구조체 값 타입이라 복사본이지만 내용이 같으므로 사실상 별칭처럼 쓸 수 있다.
	//
	// 하위 호환을 위한 SchemeGroupVersion의 별칭
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder: 이 group-version에 속한 Go 타입들을 모아 두었다가 scheme에 한 번에 등록해 주는 도구다.
	// gpuquotapolicy_types.go 같은 파일들이 각자 init()에서 SchemeBuilder.Register(...)로 자기 타입을 여기에 등록한다.
	//
	// Go 문법 설명:
	//   - &scheme.Builder{...} 의 & 는 "구조체 값을 만든 뒤 그 주소(포인터)를 얻는다"는 뜻이다.
	//     포인터여야 여러 파일이 같은 Builder 인스턴스 하나에 타입을 누적해 등록할 수 있다.
	//     값으로 두면 각자 복사본에 등록하게 되어 등록이 사라진다.
	SchemeBuilder = &scheme.Builder{GroupVersion: SchemeGroupVersion}

	// AddToScheme: 위 Builder에 모인 타입들을 실제 runtime.Scheme에 밀어 넣는 함수다.
	// cmd/main.go와 cmd/gateway/main.go가 platformv1.AddToScheme(scheme) 형태로 호출한다.
	//
	// Go 문법 설명:
	//   - Go에서 함수(메서드)는 "값"이라서 변수에 담을 수 있다.
	//   - 여기서 SchemeBuilder.AddToScheme 뒤에 괄호가 없는 점이 중요하다.
	//     괄호가 없으면 "호출"이 아니라 "그 메서드 자체를 값으로 꺼내기"이며, 이를 메서드 값(method value)이라 한다.
	//   - 꺼낸 메서드 값은 리시버(SchemeBuilder)를 기억하고 있어서 나중에 AddToScheme(scheme)로 호출하면
	//     SchemeBuilder.AddToScheme(scheme)을 부른 것과 똑같이 동작한다.
	//
	// 설계 근거: scheme에 타입이 등록되어야 client가 GPUQuotaPolicy 같은 CRD를 직렬화/역직렬화할 수 있다.
	// 등록하지 않은 타입을 읽으려 하면 "no kind is registered" 런타임 에러가 난다.
	//
	// 이 group-version의 type들을 주어진 scheme에 추가
	AddToScheme = SchemeBuilder.AddToScheme
)
