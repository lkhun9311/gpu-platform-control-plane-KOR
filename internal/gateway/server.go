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

// Package gateway는 tenant를 인식하는 OpenAI 호환 서빙 gateway 구현이다.
//
// Go 문법 설명:
//   - package 선언 바로 위에 붙은 주석은 "패키지 주석"이라 부르며, 이 패키지 전체를 설명한다.
//   - 관례상 "Package <이름>은 ..." 형태로 시작하고, go doc 문서에 그대로 노출된다.
//   - 같은 디렉터리(internal/gateway)의 모든 .go 파일은 같은 package 이름(gateway)을 공유한다.
//     그래서 이 파일의 Server 타입을 ratelimit.go나 tenant.go에서 import 없이 바로 쓸 수 있다.
//   - 경로가 internal/ 아래에 있으면 Go 컴파일러가 외부 모듈의 import를 막는다.
//     즉 이 패키지는 이 프로젝트 안에서만 쓰이는 비공개 구현이라는 뜻이다.
package gateway

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티/쿠버네티스, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 요청의 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	// 쿠버네티스 클라이언트 호출은 중단 가능해야 하므로 전부 ctx를 첫 인자로 받는다.
	"context"
	// fmt: 문자열 포맷팅 표준 패키지이며, 여기서는 fmt.Errorf로 에러에 맥락을 덧붙일 때 쓴다.
	"fmt"
	// net/http: Go의 표준 HTTP 서버/클라이언트 패키지다.
	// Handler, ServeMux, ResponseWriter, 상태 코드 상수가 전부 여기서 온다.
	"net/http"
	// sync/atomic: 잠금 없이 안전하게 읽고 쓰는 원자적(atomic) 타입 모음이다.
	// 여기서는 readiness 플래그를 담는 atomic.Bool을 쓴다.
	"sync/atomic"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(InferenceDeployment 등)이다.
	// import 경로 앞의 platformv1은 별칭(alias)이며, 원래 패키지 이름 v1이 다른 v1들과 헷갈리기 때문에 붙였다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	// runtime: 쿠버네티스의 Scheme(=Go 타입과 API 그룹/버전을 연결하는 등록부) 타입이 들어 있다.
	"k8s.io/apimachinery/pkg/runtime"
	// rest: apiserver 접속 정보(주소, 인증 정보 등)를 담는 rest.Config 타입을 제공한다.
	"k8s.io/client-go/rest"
	// cache: controller-runtime의 watch 기반 읽기 cache다.
	// apiserver를 매번 호출하지 않고 로컬 메모리에서 객체를 읽게 해준다.
	"sigs.k8s.io/controller-runtime/pkg/cache"
	// client: controller-runtime의 통합 클라이언트 인터페이스(Get/List/Create 등)를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ModelNameIndex: InferenceDeployment.spec.model.name에 대한 cache field index의 key다.
// routing은 이 index로 요청된 model을 해당 Service로 해석하며, CR field selector나 요청별 apiserver 호출이 없다.
//
// Go 문법 설명:
//   - const 는 컴파일 시점에 값이 고정되는 상수 선언 키워드다.
//     변수(var)와 달리 실행 중에 바뀔 수 없어서, 오타로 값이 흔들릴 여지가 없다.
//   - 대문자로 시작하는 이름(ModelNameIndex)은 패키지 밖에서도 보이는 "공개(export)"다.
//     index를 등록하는 쪽과 index로 조회하는 쪽이 반드시 같은 문자열을 써야 하므로 상수로 공개해 공유한다.
//
// 설계 근거(설계서 Components 절): index key 문자열을 양쪽에 하드코딩하면 한쪽만 고쳤을 때 조회가 조용히 실패한다.
// 상수 하나로 묶어 두면 그런 어긋남이 애초에 생기지 않는다.
const ModelNameIndex = ".spec.model.name"

// Server: gateway의 공유 의존성과 HTTP handler를 보유하는 구조체다.
//
// Go 문법 설명:
//   - type 이름 struct { ... } 는 여러 필드를 묶는 사용자 정의 타입(구조체) 선언이다.
//   - 이 구조체의 인스턴스 하나를 프로세스 전체가 공유하고, 모든 HTTP 요청이 그것을 동시에 읽는다.
//   - 그래서 아래 메서드들은 전부 값이 아닌 포인터 리시버(*Server)를 받는다.
//     값 리시버로 받으면 구조체가 복사되어 ready 플래그 변경이 원본에 반영되지 않는다.
type Server struct {
	// Client: scope가 지정된 cache에서 InferenceDeployment, GPUQuotaPolicy, api-keys Secret을 읽는다.
	//
	// Go 문법 설명: client.Client는 구조체가 아니라 인터페이스(interface) 타입이다.
	// 인터페이스는 "이런 메서드들을 가진 무엇이든 받는다"는 계약이라, 구현체를 자유롭게 갈아끼울 수 있다.
	// 덕분에 운영에서는 진짜 cache 기반 client를, 테스트에서는 fake client를 넣어도 코드가 그대로 동작한다.
	Client client.Client
	// Namespace와 APIKeySecret은 tenant 해석에 쓰는 api-keys Secret의 위치를 지정한다.
	//
	// Go 문법 설명: 같은 타입(string)의 필드는 이렇게 줄을 나눠 연달아 선언할 수 있다.
	// Secret 이름과 namespace를 코드에 박지 않고 필드로 주입받는 이유는 배포 환경마다 값이 다르기 때문이다.
	Namespace    string
	APIKeySecret string
	// ready: cache가 동기화되면 true로 뒤집히며 readiness를 gating하는 플래그다.
	//
	// Go 문법 설명:
	//   - atomic.Bool은 여러 goroutine이 동시에 읽고 써도 안전한 불리언 타입이다.
	//   - 평범한 bool 필드를 쓰면 한쪽에서 쓰고 다른 쪽에서 읽을 때 data race가 되어 동작이 정의되지 않는다.
	//   - 값을 넣을 땐 Store(true), 읽을 땐 Load()처럼 반드시 메서드를 거친다(= 대입 연산자를 쓰지 않는다).
	//   - 소문자로 시작하므로 패키지 밖에서는 직접 건드릴 수 없고, 아래 MarkReady를 통해서만 바꿀 수 있다.
	//
	// 설계 근거(설계서 Components 절): cache 동기화를 담당하는 goroutine이 이 값을 쓰고,
	// /readyz를 처리하는 HTTP goroutine들이 동시에 이 값을 읽는다.
	// 쓰는 쪽과 읽는 쪽이 다른 goroutine이므로 atomic이 반드시 필요하다.
	ready atomic.Bool
}

// markReady: gateway가 서빙 가능한 상태임을 표시하는 내부 헬퍼다.
//
// Go 문법 설명:
//   - func (s *Server) 부분이 리시버(receiver)이며, 이 함수가 Server 타입에 붙는 "메서드"라는 뜻이다.
//   - Store(true)는 ready 플래그에 true를 원자적으로 기록한다.
//   - 소문자 이름이라 패키지 내부에서만 호출할 수 있으며, 같은 패키지인 테스트 코드는 이걸 직접 부를 수 있다.
//   - 본문이 짧으면 이렇게 한 줄로 붙여 써도 되며, gofmt도 이 형태를 그대로 둔다.
func (s *Server) markReady() { s.ready.Store(true) }

// MarkReady: cache의 첫 동기화 이후 binary가 readiness를 뒤집기 위한 exported 진입점이다.
//
// Go 문법 설명: 대문자로 시작하므로 다른 패키지(cmd/의 main 등)에서 호출할 수 있는 공개 메서드다.
// 하는 일은 소문자 markReady를 그대로 부르는 것뿐이라 얼핏 중복처럼 보인다.
// 하지만 "공개 API 표면"과 "내부 구현"을 분리해 두면, 나중에 markReady의 동작이 복잡해져도
// 바깥에 노출된 이름과 시그니처는 흔들리지 않는다.
func (s *Server) MarkReady() { s.markReady() }

// readyz: cache가 동기화된 후에만 200을 반환하고, 아니면 503을 반환해 Pod가 Service endpoint에서 빠지도록 한다.
//
// Go 문법 설명:
//   - 이 시그니처(w http.ResponseWriter, r *http.Request)는 Go의 HTTP handler 표준 형태다.
//     이 모양을 갖추면 아래 mux.HandleFunc에 그대로 넘길 수 있다.
//   - w는 응답을 쓰는 통로이고, r은 들어온 요청이다.
//   - 두 번째 인자 이름이 밑줄(_)인 것은 "받긴 하지만 쓰지 않는다"는 의미다.
//     readyz는 요청 내용과 무관하게 판단하므로 Request를 볼 필요가 없다.
//     인자 자리를 비울 수는 없어서(시그니처가 고정) 이름만 _로 버린다.
//
// 설계 근거(설계서 Components 절): cache가 아직 채워지지 않았는데 트래픽을 받으면
// "model을 못 찾음"이나 "정책 없음" 같은 잘못된 오류를 사용자에게 돌려주게 된다.
// 503을 반환하면 kubelet의 readiness probe가 실패하고, 쿠버네티스가 이 Pod를 Service endpoint 목록에서 빼준다.
// 즉 준비되지 않은 Pod에는 애초에 요청이 도달하지 않는다.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	// Load()로 현재 ready 값을 원자적으로 읽는다.
	// ! 는 "부정(NOT)"이므로 이 조건은 "아직 준비되지 않았다면"이라는 뜻이다.
	if !s.ready.Load() {
		// http.Error는 상태 코드와 본문 메시지를 한 번에 써주는 표준 헬퍼다.
		// StatusServiceUnavailable은 503을 뜻하는 상수이며, 숫자 대신 상수를 쓰면 의도가 드러나고 오타도 막힌다.
		http.Error(w, "cache not synced", http.StatusServiceUnavailable)
		// return으로 여기서 함수를 끝낸다.
		// 이게 없으면 아래 200 쓰기까지 이어져 한 응답에 상태 코드를 두 번 쓰는 버그가 된다.
		return
	}
	// 준비된 경우엔 본문 없이 200만 돌려준다.
	// WriteHeader는 상태 코드만 기록하며, probe는 코드만 보므로 본문이 필요 없다.
	w.WriteHeader(http.StatusOK)
}

// Handler: :8080에서 서빙하는 mux를 만들어 돌려준다.
// 이후 작업에서 POST /v1/chat/completions를 추가할 예정이다.
//
// Go 문법 설명:
//   - 반환 타입 http.Handler는 인터페이스이며, ServeHTTP 메서드를 가진 무엇이든 담을 수 있다.
//     구체 타입(*http.ServeMux) 대신 인터페이스를 반환하면 호출한 쪽이 내부 구현에 묶이지 않는다.
//   - http.NewServeMux()는 "경로 → handler" 라우팅 표를 만든다.
//   - mux.HandleFunc(경로, 함수)는 그 표에 한 줄을 등록한다.
//   - s.readyz 처럼 괄호 없이 메서드 이름만 쓰면 "호출"이 아니라 "함수 값 자체"를 넘기는 것이다.
//     이때 리시버 s가 함께 묶여서(method value) 나중에 호출될 때도 같은 Server 인스턴스를 본다.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// MetricsHandler: :8081에서 관측성을 담당하는 mux를 만들어 돌려준다.
// 이후 작업에서 /metrics를 추가할 예정이다.
//
// 설계 근거(설계서 Components 절): 사용자 트래픽(:8080)과 관측성 트래픽(:8081)을 별도 포트로 분리한다.
// 포트가 나뉘어 있으면 metrics 엔드포인트를 외부에 노출하지 않고 클러스터 내부에만 열어둘 수 있다.
// 두 mux 모두 /readyz를 등록하는 이유는 어느 포트로 probe를 걸든 같은 답을 얻게 하기 위해서다.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// NewCache: gateway의 읽기 집합에 대한 scope cache와, cache를 읽고 apiserver에 쓰는 위임 client를 생성한다.
// 그리고 routing에 쓰는 model-name field indexer를 등록한다.
//
// Go 문법 설명:
//   - 리시버가 없는 일반 함수다(특정 타입에 붙지 않음).
//   - 반환 타입 (cache.Cache, client.Client, error)처럼 Go는 값을 여러 개 돌려줄 수 있다.
//     관례상 마지막 반환값을 error로 두고, 에러가 없으면 nil을 넣는다.
//   - cache와 client를 둘 다 돌려주는 이유는 역할이 다르기 때문이다.
//     cache는 호출한 쪽이 Start로 돌리고 동기화를 기다려야 하는 대상이고, client는 실제로 객체를 읽고 쓰는 도구다.
//
// 설계 근거(설계서 Components 절): 게이트웨이는 요청마다 apiserver를 때리면 안 된다.
// watch 기반 cache를 로컬에 두고 메모리에서 읽어야 지연이 낮고 apiserver 부하도 없다.
func NewCache(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (cache.Cache, client.Client, error) {
	// cache.New(...)는 접속 설정(cfg)과 타입 등록부(scheme)로 cache를 만든다.
	// ca, err := ... 처럼 반환값이 둘이면 왼쪽에 변수를 둘 나열해 받는다.
	// cache.Options의 두 필드 설명:
	//   - Scheme: 어떤 Go 타입이 어떤 API 그룹/버전에 대응하는지 알려주는 등록부다.
	//     이게 없으면 cache가 InferenceDeployment 같은 커스텀 타입을 역직렬화하지 못한다.
	//   - DefaultTransform: cache에 넣기 전에 객체를 한 번 가공하는 함수다.
	//     TransformStripManagedFields()는 metadata.managedFields를 떼어낸다.
	//     이 필드는 서버 사이드 apply 이력이라 게이트웨이가 전혀 쓰지 않으면서 객체마다 용량을 크게 차지한다.
	//     미리 버리면 cache 메모리 사용량이 눈에 띄게 줄어든다.
	ca, err := cache.New(cfg, cache.Options{
		Scheme:           scheme,
		DefaultTransform: cache.TransformStripManagedFields(),
	})
	// 에러가 있으면 두 개의 반환값 자리에 nil을 채우고 에러만 위로 올린다.
	if err != nil {
		// fmt.Errorf의 %w 동사는 원래 에러를 "감싸서(wrap)" 새 에러 안에 보존한다.
		// %v로 문자열만 붙이면 원인 에러가 사라지지만, %w를 쓰면 나중에 errors.Is/As로 원인을 다시 꺼낼 수 있다.
		// 앞에 "new cache: "를 붙여 어느 단계에서 깨졌는지 맥락을 남긴다.
		return nil, nil, fmt.Errorf("new cache: %w", err)
	}
	// routing 조회를 위해 InferenceDeployment를 그것이 서빙하는 model 이름으로 index한다.
	//
	// Go 문법 설명:
	//   - IndexField의 마지막 인자는 함수 리터럴(익명 함수)이다.
	//     이름 없이 func(...) ... { ... } 형태로 그 자리에서 함수를 정의해 값으로 넘긴다.
	//   - 이 함수는 객체 하나를 받아 "이 객체를 어떤 key들로 찾을 수 있는지"를 문자열 슬라이스로 답한다.
	//     반환이 슬라이스인 이유는 한 객체가 여러 key를 가질 수도 있기 때문이며, 여기서는 항상 1개다.
	//   - o.(*platformv1.InferenceDeployment)는 타입 단언(type assertion)이다.
	//     인자 o는 임의의 쿠버네티스 객체를 담는 client.Object 인터페이스라 구체 타입으로 되돌려야 Spec에 접근할 수 있다.
	//     반환값 하나로 받는 이 형태는 타입이 다르면 패닉이 나지만, 이 indexer는 InferenceDeployment에만 등록되므로 안전하다.
	//   - &platformv1.InferenceDeployment{}는 값이 빈 객체이며, 내용이 아니라 "어떤 타입에 index를 걸지"만 알려주는 용도다.
	//
	// 설계 근거(설계서 Request flow 절): index가 없으면 요청마다 전체 InferenceDeployment를 훑어야 한다.
	// index를 걸어두면 model 이름으로 즉시 조회되므로 배포 개수가 늘어도 조회 비용이 일정하다.
	if err := ca.IndexField(ctx, &platformv1.InferenceDeployment{}, ModelNameIndex, func(o client.Object) []string {
		return []string{o.(*platformv1.InferenceDeployment).Spec.Model.Name}
	}); err != nil {
		// index key 이름을 에러에 함께 남겨, 여러 index 중 어느 것이 실패했는지 바로 알 수 있게 한다.
		return nil, nil, fmt.Errorf("index %s: %w", ModelNameIndex, err)
	}
	// client.New(...)로 "위임(delegating) client"를 만든다.
	//
	// Go 문법 설명: client.Options의 Cache 필드에 &client.CacheOptions{Reader: ca}를 넣는 것이 핵심이다.
	// 이렇게 하면 읽기(Get/List)는 방금 만든 cache(ca)로 가고, 쓰기(Create/Update)는 apiserver로 직접 간다.
	// 이 필드를 비워두면 모든 읽기가 apiserver를 때려서 cache를 만든 의미가 사라진다.
	cl, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: ca}})
	if err != nil {
		return nil, nil, fmt.Errorf("new delegating client: %w", err)
	}
	// 정상 경로: cache와 client를 돌려주고, 마지막 자리엔 "에러 없음"을 뜻하는 nil을 넣는다.
	// 여기서 반환된 cache는 아직 시작되지 않았으므로, 호출한 쪽이 Start를 부르고 동기화를 기다린 뒤 MarkReady를 호출해야 한다.
	return ca, cl, nil
}
