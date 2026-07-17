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
	// errors: errors.Is로 ErrNoPolicy/ErrNoRoute 같은 특정 에러를 정확히 골라낼 때 쓴다.
	"errors"
	// fmt: 문자열 포맷팅 표준 패키지이며, 여기서는 fmt.Errorf로 에러에 맥락을 덧붙일 때 쓴다.
	"fmt"
	// net/http: Go의 표준 HTTP 서버/클라이언트 패키지다.
	// Handler, ServeMux, ResponseWriter, 상태 코드 상수가 전부 여기서 온다.
	"net/http"
	// net/url: backend 주소를 담는 URL 타입이다.
	"net/url"
	// strconv: 상태 코드(int)를 metric 라벨(string)로 바꿀 때 쓴다.
	"strconv"
	// sync/atomic: 잠금 없이 안전하게 읽고 쓰는 원자적(atomic) 타입 모음이다.
	// 여기서는 readiness 플래그를 담는 atomic.Bool을 쓴다.
	"sync/atomic"
	// time: 요청 처리 시간을 재는 데 쓴다.
	"time"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(InferenceDeployment 등)이다.
	// import 경로 앞의 platformv1은 별칭(alias)이며, 원래 패키지 이름 v1이 다른 v1들과 헷갈리기 때문에 붙였다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	// corev1: 쿠버네티스 내장 타입들이며, 여기서는 Secret의 cache 감시 범위를 지정할 때 쓴다.
	corev1 "k8s.io/api/core/v1"
	// runtime: 쿠버네티스의 Scheme(=Go 타입과 API 그룹/버전을 연결하는 등록부) 타입이 들어 있다.
	"k8s.io/apimachinery/pkg/runtime"
	// rest: apiserver 접속 정보(주소, 인증 정보 등)를 담는 rest.Config 타입을 제공한다.
	"k8s.io/client-go/rest"
	// cache: controller-runtime의 watch 기반 읽기 cache다.
	// apiserver를 매번 호출하지 않고 로컬 메모리에서 객체를 읽게 해준다.
	"sigs.k8s.io/controller-runtime/pkg/cache"
	// client: controller-runtime의 통합 클라이언트 인터페이스(Get/List/Create 등)를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// log: context에 실린 logger를 꺼내 쓰는 controller-runtime의 로깅 진입점이다.
	// router.go가 이미 같은 방식으로 쓰고 있어 로그 형식이 패키지 안에서 일관된다.
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	// buckets: tenant별 token bucket을 보관하는 등록부다.
	//
	// Go 문법 설명: 소문자로 시작하므로 패키지 밖에서는 보이지 않는다.
	// 이 필드를 채우는 일은 cmd/gateway/main.go의 조립 단계가 맡는다.
	//
	// 설계 근거(설계서 Components 절): 속도 제한은 요청 사이에 상태(남은 토큰)가 이어져야 의미가 있다.
	// 요청마다 새 bucket을 만들면 모든 요청이 가득 찬 bucket을 보게 되어 제한이 전혀 걸리지 않는다.
	// 그래서 프로세스 전체가 공유하는 이 등록부 하나에 tenant별 bucket을 모아 둔다.
	buckets *bucketRegistry
	// backendOverride: model을 backend URL로 바꾸는 경로를 테스트에서 갈아끼우는 훅이다.
	// nil이면(운영에서는 항상 nil이다) 아래 resolveBackend가 진짜 backendFor를 쓴다.
	//
	// 왜 이런 훅이 필요한가(플랜 Task 6):
	// backendFor는 http://<name>.<ns>.svc:<port> 라는 클러스터 내부 DNS 주소를 만든다.
	// 테스트 프로세스에는 그 DNS가 없으므로 어떤 방법으로도 그 주소에 붙을 수 없다.
	// 훅을 두면 해석 결과만 httptest 서버 주소로 바꿔 파이프라인 전체를 실제로 통과시킬 수 있다.
	// 훅이 nil일 때 운영 경로가 그대로 도는 것이 핵심이다. 즉 이 훅은 운영 동작을 우회하지 않는다.
	backendOverride func(model string) *url.URL
	// responseHeaderTimeout: 업스트림의 응답 헤더를 기다리는 상한이다.
	// 0이면 proxy.go의 defaultResponseHeaderTimeout(30초)이 쓰이므로 운영에서는 비워 둔다.
	//
	// 왜 상수로 박지 않고 필드로 뺐는가:
	// 이 값은 504 응답이 나오기까지 걸리는 시간을 그대로 결정한다.
	// 30초로 고정하면 504 매핑을 검증하는 테스트가 30초를 기다려야 하므로 아무도 그 테스트를 두지 않게 되고,
	// 결국 504 분기가 검증되지 않은 채 남는다(그 분기는 지워져도 502로 조용히 대체될 뿐이라 더 위험하다).
	// 필드로 두면 테스트가 같은 코드 경로를 짧은 상한으로 즉시 통과시킬 수 있다.
	responseHeaderTimeout time.Duration
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

// InitRateLimiter: tenant별 token bucket 등록부를 만들어 넣는 exported 진입점이다.
//
// 왜 이 메서드가 필요한가:
// buckets 필드도 bucketRegistry 타입도 소문자라 이 패키지 밖에서는 보이지 않는다.
// 그래서 cmd/gateway/main.go는 구조체 리터럴로 직접 채울 수 없고, 이렇게 공개된 메서드를 거쳐야 한다.
// 타입을 공개하지 않고 감춰 두는 편이 나은 이유는, bucket의 내부 구조가 바뀌어도
// 조립하는 쪽 코드는 전혀 건드릴 필요가 없기 때문이다.
func (s *Server) InitRateLimiter() { s.buckets = newBucketRegistry() }

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

// fail: 요청을 주어진 상태 코드로 끝내고 그 사실을 metric에 남긴다.
//
// 왜 헬퍼로 묶는가:
// 파이프라인의 모든 실패 분기가 "코드를 쓴다 + metric을 센다" 두 가지를 함께 해야 한다.
// 한 곳이라도 metric을 빠뜨리면 그 실패는 관측되지 않아 조용히 사라진다.
// 헬퍼 하나를 거치게 하면 그런 누락이 애초에 생기지 않는다.
//
// model 인자가 빈 문자열일 수 있는 이유: 인증/정책/속도 제한 단계는 본문을 파싱하기 전이라 model을 아직 모른다.
// 그때는 ""를 넣어 "이 단계에서는 model이 정해지지 않았다"는 사실이 metric에 그대로 드러나게 한다.
func (s *Server) fail(w http.ResponseWriter, tenant, model string, code int) {
	requests.WithLabelValues(tenant, model, strconv.Itoa(code)).Inc()
	http.Error(w, http.StatusText(code), code)
}

// resolveBackend: model을 backend URL로 해석한다.
// 테스트 훅이 걸려 있으면 그것을 쓰고, 아니면 진짜 backendFor를 쓴다.
//
// Go 문법 설명: 함수 타입 필드가 nil인지 검사하는 것은 "훅이 설정되었는가"를 묻는 관용구다.
// nil인 함수를 그냥 호출하면 패닉이 나므로 이 검사가 반드시 앞에 와야 한다.
func (s *Server) resolveBackend(ctx context.Context, policy *platformv1.GPUQuotaPolicy, model string) (*url.URL, error) {
	if s.backendOverride != nil {
		return s.backendOverride(model), nil
	}
	return s.backendFor(ctx, policy, model)
}

// chatCompletions: OpenAI 호환 chat completions 요청을 처리하는 파이프라인이다.
//
// 단계의 순서가 이 함수의 핵심이다(설계서 Request flow 절):
//  1. request id 부여 — 이후 모든 로그와 업스트림 요청이 같은 id를 공유해야 추적이 이어진다.
//  2. 인증 — 누구인지 모르는 요청은 여기서 끝난다(401).
//  3. 정책 조회 — 신원은 알지만 권한이 없으면 여기서 끝난다(403).
//  4. 속도 제한 — 자기 몫을 넘겼으면 여기서 끝난다(429).
//  5. 본문 파싱 — model을 꺼낸다(400).
//  6. 라우팅 — model을 backend로 해석한다(404).
//  7. 프록시 — 업스트림으로 넘긴다(502/504 또는 업스트림의 응답).
//
// 왜 인증이 속도 제한보다 먼저인가:
// 속도 제한은 tenant별 bucket을 쓰므로 tenant를 모르면 애초에 판정할 수 없다.
// 게다가 인증 없이 제한을 걸면 익명 요청이 남의 bucket을 소모시켜, 공격자가 남의 tenant를 마비시킬 수 있다.
//
// 왜 속도 제한이 본문 파싱보다 먼저인가:
// 본문 파싱은 최대 1MB를 메모리에 올린다. 제한에 걸릴 요청까지 본문을 읽으면
// 폭주하는 클라이언트가 게이트웨이 메모리를 계속 소모시킬 수 있다.
// 거절할 요청은 가능한 한 빨리, 비용을 쓰기 전에 거절하는 것이 맞다.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. request id: 호출자가 이미 달아 왔으면 그대로 쓰고, 없으면 새로 만든다.
	//
	// 왜 들어온 id를 이어받는가: 호출자가 자기 시스템의 추적 id를 달아 보냈다면
	// 그것을 유지해야 게이트웨이의 로그와 호출자의 로그가 하나로 이어진다.
	// 무조건 새로 만들면 그 연결이 끊긴다.
	rid := r.Header.Get("X-Request-Id")
	if rid == "" {
		rid = newRequestID()
	}
	// 응답과 업스트림 요청 양쪽에 단다.
	// 응답에만 달면 업스트림 로그에서 이 요청을 찾을 수 없고,
	// 요청에만 달면 클라이언트가 자기 요청의 id를 알 수 없어 문의 시 대조가 불가능하다.
	w.Header().Set("X-Request-Id", rid)
	r.Header.Set("X-Request-Id", rid)

	// 2. 인증: API key를 tenant로 해석한다.
	tenant, ok := s.resolveTenant(ctx, r)
	if !ok {
		s.fail(w, "", "", http.StatusUnauthorized)
		return
	}

	// 3. 정책 조회: 이 tenant의 GPUQuotaPolicy를 찾는다.
	policy, err := s.policyForTenant(ctx, tenant)
	// errors.Is로 "정책이 없음"이라는 정상적 결과와 "조회가 깨짐"이라는 사고를 구분한다.
	// 이 둘을 뭉뚱그리면 apiserver 장애가 403으로 보여 운영자가 정책 설정을 헤매게 된다.
	if errors.Is(err, ErrNoPolicy) {
		s.fail(w, tenant, "", http.StatusForbidden)
		return
	}
	if err != nil {
		// 조회 자체가 실패한 경우다. 클라이언트 잘못이 아니므로 502다.
		log.FromContext(ctx).Error(err, "policy lookup failed", "tenant", tenant, "request_id", rid)
		s.fail(w, tenant, "", http.StatusBadGateway)
		return
	}

	// 4. 속도 제한: tenant의 bucket에서 토큰 하나를 꺼내 본다.
	if !s.buckets.Allow(tenant, policy.Spec.RateLimit) {
		// 전용 metric을 따로 센다(requests의 code=429와 목적이 다르다).
		rateLimited.WithLabelValues(tenant).Inc()
		s.fail(w, tenant, "", http.StatusTooManyRequests)
		return
	}

	// 5. 본문 파싱: model을 꺼내고 본문을 복원한다.
	body, model, err := readModel(r)
	if err != nil {
		// 깨진 JSON, model 누락, 크기 초과가 모두 여기 해당한다.
		// 셋 다 클라이언트가 고쳐야 할 문제이므로 400이 맞다.
		s.fail(w, tenant, "", http.StatusBadRequest)
		return
	}
	// 복원된 본문을 요청에 되돌려 놓는다. 이 줄이 없으면 업스트림이 빈 본문을 받는다.
	r.Body = body

	// 6. 라우팅: model을 서빙하는 backend를 찾는다.
	target, err := s.resolveBackend(ctx, policy, model)
	if errors.Is(err, ErrNoRoute) {
		// 그런 model이 없다는 정상적 결과다.
		s.fail(w, tenant, model, http.StatusNotFound)
		return
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "backend lookup failed", "tenant", tenant, "model", model, "request_id", rid)
		s.fail(w, tenant, model, http.StatusBadGateway)
		return
	}

	// 7. 프록시: 여기서부터는 응답을 우리가 만들지 않고 업스트림 것을 그대로 흘려보낸다.
	start := time.Now()
	// statusRecorder로 감싸 업스트림이 쓴 상태 코드를 나중에 metric에 남길 수 있게 한다.
	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	newReverseProxy(target, s.responseHeaderTimeout, func(c int) {
		upstreamErrors.WithLabelValues(tenant, model).Inc()
		// ErrorHandler가 http.Error로 코드를 쓰지만 그 경로는 statusRecorder를 거치지 않을 수 있으므로
		// 여기서 직접 기록해 metric이 실제 응답과 어긋나지 않게 한다.
		rec.code = c
	}).ServeHTTP(rec, r)

	// ServeHTTP가 반환했다는 것은 응답 전송이 끝났다는 뜻이다(스트리밍이면 스트림이 닫힌 시점이다).
	// 그러므로 여기서 재는 시간은 첫 바이트까지가 아니라 요청 전체가 완료되기까지의 시간이다.
	requests.WithLabelValues(tenant, model, strconv.Itoa(rec.code)).Inc()
	requestDuration.WithLabelValues(tenant, model).Observe(time.Since(start).Seconds())
}

// Handler: :8080에서 사용자 트래픽을 서빙하는 mux를 만들어 돌려준다.
//
// Go 문법 설명:
//   - 반환 타입 http.Handler는 인터페이스이며, ServeHTTP 메서드를 가진 무엇이든 담을 수 있다.
//     구체 타입(*http.ServeMux) 대신 인터페이스를 반환하면 호출한 쪽이 내부 구현에 묶이지 않는다.
//   - http.NewServeMux()는 "경로 → handler" 라우팅 표를 만든다.
//   - mux.HandleFunc(패턴, 함수)는 그 표에 한 줄을 등록한다.
//   - s.readyz 처럼 괄호 없이 메서드 이름만 쓰면 "호출"이 아니라 "함수 값 자체"를 넘기는 것이다.
//     이때 리시버 s가 함께 묶여서(method value) 나중에 호출될 때도 같은 Server 인스턴스를 본다.
//
// 패턴에 메서드를 함께 적는 이유(설계서 Error codes 절):
// Go 1.22부터 ServeMux 패턴에 "POST /경로"처럼 메서드를 적을 수 있다.
// 이렇게 등록하면 경로는 맞고 메서드만 다른 요청에 mux가 알아서 405를 돌려주고,
// 등록되지 않은 경로에는 404를 돌려준다. 즉 두 코드를 우리가 직접 구현할 필요가 없다.
// 메서드 없이 "/v1/chat/completions"로만 등록하면 GET 요청까지 파이프라인으로 들어와
// 405여야 할 것이 401이나 400으로 나가게 된다.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// MetricsHandler: :8081에서 관측성을 담당하는 mux를 만들어 돌려준다.
//
// 설계 근거(설계서 Components 절): 사용자 트래픽(:8080)과 관측성 트래픽(:8081)을 별도 포트로 분리한다.
// 포트가 나뉘어 있으면 metrics 엔드포인트를 외부에 노출하지 않고 클러스터 내부에만 열어둘 수 있다.
// tenant별 사용량이 담긴 metric이 사용자에게 노출되면 다른 tenant의 활동을 추측할 수 있으므로 반드시 분리해야 한다.
// 두 mux 모두 /readyz를 등록하는 이유는 어느 포트로 probe를 걸든 같은 답을 얻게 하기 위해서다.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHTTPHandler())
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
func NewCache(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme, namespace string) (cache.Cache, client.Client, error) {
	// cache.New(...)는 접속 설정(cfg)과 타입 등록부(scheme)로 cache를 만든다.
	// ca, err := ... 처럼 반환값이 둘이면 왼쪽에 변수를 둘 나열해 받는다.
	// cache.Options의 세 필드 설명:
	//   - Scheme: 어떤 Go 타입이 어떤 API 그룹/버전에 대응하는지 알려주는 등록부다.
	//     이게 없으면 cache가 InferenceDeployment 같은 커스텀 타입을 역직렬화하지 못한다.
	//   - DefaultTransform: cache에 넣기 전에 객체를 한 번 가공하는 함수다.
	//     TransformStripManagedFields()는 metadata.managedFields를 떼어낸다.
	//     이 필드는 서버 사이드 apply 이력이라 게이트웨이가 전혀 쓰지 않으면서 객체마다 용량을 크게 차지한다.
	//     미리 버리면 cache 메모리 사용량이 눈에 띄게 줄어든다.
	//   - ByObject: 타입마다 감시 범위를 따로 정한다. 아래에서 Secret에만 건다.
	ca, err := cache.New(cfg, cache.Options{
		Scheme:           scheme,
		DefaultTransform: cache.TransformStripManagedFields(),
		// Secret은 이 게이트웨이가 떠 있는 namespace 하나만 감시한다.
		//
		// 이 범위 제한이 없으면 무슨 일이 벌어지는가(설계서 Components 절이 "scoped cache"를 요구하는 이유):
		// controller-runtime의 cache는 범위를 지정하지 않으면 모든 namespace를 감시한다
		// (cache.Options 문서: "An empty map ... means that all namespaces will be cached").
		// 그러면 게이트웨이가 클러스터의 모든 Secret을 메모리에 들고 있게 된다.
		// 거기엔 다른 컴포넌트의 자격증명과 TLS 개인키까지 전부 포함된다.
		// 게이트웨이는 외부 트래픽을 직접 받는 유일한 컴포넌트라 침해 시 그 전부가 함께 새어 나간다.
		// 필요한 것은 api-keys Secret 하나뿐이므로 그 namespace로 가둔다.
		//
		// RBAC과 반드시 짝이 맞아야 한다:
		// config/gateway/rbac.yaml은 secrets 읽기를 게이트웨이 namespace의 Role로만 준다.
		// 여기서 범위를 가두지 않으면 cache가 모든 namespace의 secrets를 list/watch 하려다 권한이 없어 실패하고,
		// cache가 영영 동기화되지 않아 readiness가 열리지 않는다. 즉 게이트웨이가 아무 요청도 받지 못한다.
		//
		// InferenceDeployment는 여기에 넣지 않는다. tenant마다 다른 namespace에 있고
		// 게이트웨이는 어떤 tenant의 요청이든 받아야 하므로 범위를 미리 좁힐 수 없다.
		// GPUQuotaPolicy는 cluster-scoped라 애초에 namespace 개념이 없다.
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					namespace: {},
				},
			},
		},
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
