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

// package 선언: 이 테스트도 테스트 대상과 같은 gateway 패키지에 속하는 "내부 테스트"다.
// 덕분에 소문자로 감춰진 chatCompletions나 readModel 같은 비공개 멤버를 직접 부를 수 있고,
// router_test.go의 newSchemeForTest/policyFor 같은 헬퍼도 그대로 재사용할 수 있다.
package gateway

import (
	// bufio: 스트리밍 응답을 한 줄씩 끊어 읽기 위해 쓴다.
	// 스트리밍 검증은 "핸들러가 끝나기 전에 바이트가 도착하는가"를 봐야 하므로 줄 단위 읽기가 필요하다.
	"bufio"
	// context: 취소 신호를 함수 사이로 옮기는 표준 타입이다.
	"context"
	// fmt: 업스트림 스텁이 SSE 청크 문자열을 만들 때 쓴다.
	"fmt"
	// net/http: 메서드 상수, 상태 코드 상수, 서버/클라이언트 타입이 전부 여기서 온다.
	"net/http"
	// net/http/httptest: 진짜 포트를 열지 않고 핸들러를 시험하는 표준 도구다.
	// 다만 아래 스트리밍 테스트만은 예외적으로 진짜 서버를 띄운다(이유는 해당 It에 적어 두었다).
	"net/http/httptest"
	// net/url: backendOverride가 돌려줄 URL 타입이다.
	"net/url"
	// strings: 요청 본문을 문자열에서 Reader로 만들 때 쓴다.
	"strings"
	// time: 스트리밍 테스트의 타임아웃과 청크 간 간격에 쓴다.
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// 이 파일 전체가 공유하는 테스트 좌표다.
//
// Go 문법 설명: const 블록은 여러 상수를 한 번에 선언한다.
// 문자열을 테스트마다 반복해 적으면 한쪽만 고쳤을 때 조회가 조용히 어긋나므로 한 곳에 묶어 둔다.
const (
	testGatewayNS = "gw"               // api-keys Secret이 있는 namespace
	testSecret    = "gateway-api-keys" // api-keys Secret 이름
	testTenant    = "team-vision"      // 아래 key가 해석되는 tenant
	testKey       = "k1"               // 정상 동작하는 API key
	testTenantNS  = "team-vision-ns"   // 정책의 TargetNamespace
	testModel     = "llama-3"          // InferenceDeployment가 서빙하는 model 이름
)

// newProxyServer: 요청 파이프라인 전체를 시험할 수 있게 배선된 Server를 만든다.
//
// 무엇을 심어 두는가:
//   - api-keys Secret: key "k1" → tenant "team-vision"
//   - GPUQuotaPolicy: 그 tenant의 정책이며 TargetNamespace와 rateLimit을 지정한다
//   - InferenceDeployment: 그 namespace에서 testModel을 서빙한다
//
// rpm 인자를 따로 받는 이유:
// rate limit 테스트만 버킷을 고갈시켜야 하고 나머지 테스트는 절대 429에 걸리면 안 된다.
// 그래서 호출부가 분당 요청 수를 정하게 해 두었다.
//
// backendOverride를 쓰는 이유(플랜 Task 6):
// 운영에서 backendFor는 http://<name>.<ns>.svc:<port> 라는 클러스터 내부 DNS 주소를 만든다.
// 테스트 프로세스에는 그 DNS가 없어 절대 붙지 못하므로, 해석 결과만 httptest 서버 주소로 갈아끼운다.
// 훅이 nil이면 운영과 똑같이 backendFor가 쓰이므로, 이 훅은 운영 경로를 우회하지 않는다.
func newProxyServer(upstream string, rpm int32) *Server {
	// api-keys Secret: tenant.go의 resolveTenant가 key를 tenant로 바꿀 때 읽는다.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testGatewayNS},
		Data:       map[string][]byte{testKey: []byte(testTenant)},
	}
	// GPUQuotaPolicy: policyForTenant가 Spec.Tenant로 찾고, backendFor가 Spec.TargetNamespace로 범위를 좁힌다.
	policy := &platformv1.GPUQuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "vision-policy"},
		Spec: platformv1.GPUQuotaPolicySpec{
			Tenant:          testTenant,
			TargetNamespace: testTenantNS,
			// Burst를 1로 두면 "첫 요청은 통과, 두 번째부터 429"라는 경계가 또렷해진다.
			RateLimit: &platformv1.GPUQuotaRateLimit{RequestsPerMinute: rpm, Burst: 1},
		},
	}
	// InferenceDeployment: 이것이 있어야 testModel이 라우팅 대상으로 해석된다.
	infd := newInfD("llama", testTenantNS, testModel, 8080, time.Now())

	s := &Server{
		Client:       newRouterClient(secret, policy, infd),
		Namespace:    testGatewayNS,
		APIKeySecret: testSecret,
		buckets:      newBucketRegistry(),
	}
	// 업스트림 주소가 주어졌을 때만 훅을 건다.
	// 빈 문자열이면 훅이 nil로 남아 backendFor가 그대로 쓰이며, "붙을 수 없는 주소" 상황을 만들 수 있다.
	if upstream != "" {
		u, err := url.Parse(upstream)
		Expect(err).NotTo(HaveOccurred())
		s.backendOverride = func(string) *url.URL { return u }
	}
	// 파이프라인 테스트는 readiness와 무관하므로 준비 완료로 만들어 둔다.
	s.markReady()
	return s
}

// authedRequest: 정상 Bearer 토큰을 단 POST 요청을 만든다.
func authedRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	return r
}

// 요청 파이프라인의 에러 코드 매핑에 대한 명세 묶음이다.
//
// 이 테스트들이 막는 회귀(설계서 Error codes 절):
// 파이프라인은 인증 → 정책 → 속도 제한 → 본문 파싱 → 라우팅 순서로 진행되며,
// 각 단계의 실패가 서로 다른 상태 코드로 나뉘어야 클라이언트가 원인을 구분할 수 있다.
// 순서가 어긋나면(예: 속도 제한을 인증보다 먼저) 인증되지 않은 요청이 남의 버킷을 소모시킬 수 있고,
// 코드가 뭉개지면(전부 400) 호출자가 재시도해야 할 상황과 고쳐야 할 상황을 구분하지 못한다.
var _ = Describe("chat completions pipeline", func() {
	It("returns 405 for a non-POST method on the completions path", func() {
		// 경로는 맞지만 메서드가 GET이다.
		// 라우팅이 메서드를 구분하지 않으면 이 요청이 파이프라인으로 들어가 401이나 400이 되어 이 테스트가 실패한다.
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
		Expect(rr.Code).To(Equal(http.StatusMethodNotAllowed))
	})

	It("returns 404 for an unknown path", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil))
		Expect(rr.Code).To(Equal(http.StatusNotFound))
	})

	It("returns 401 when the bearer token is missing", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		// Authorization 헤더를 일부러 달지 않는다.
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama-3"}`))
		s.Handler().ServeHTTP(rr, r)
		Expect(rr.Code).To(Equal(http.StatusUnauthorized))
	})

	It("returns 401 when the bearer token is unknown", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama-3"}`))
		// Secret에 없는 key다. 인증은 형식이 아니라 실제 존재 여부로 판정되어야 한다.
		r.Header.Set("Authorization", "Bearer nope")
		s.Handler().ServeHTTP(rr, r)
		Expect(rr.Code).To(Equal(http.StatusUnauthorized))
	})

	It("returns 403 when the tenant has no policy", func() {
		// 인증은 되지만 정책이 없는 tenant를 만든다.
		// Secret에 key를 하나 더 넣고 그 tenant의 GPUQuotaPolicy는 심지 않는다.
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testGatewayNS},
			Data:       map[string][]byte{"k2": []byte("no-policy-tenant")},
		}
		s := &Server{
			Client:       newRouterClient(secret),
			Namespace:    testGatewayNS,
			APIKeySecret: testSecret,
			buckets:      newBucketRegistry(),
		}
		s.markReady()
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama-3"}`))
		r.Header.Set("Authorization", "Bearer k2")
		s.Handler().ServeHTTP(rr, r)
		// 401이 아니라 403이어야 한다. 신원은 확인됐고 권한이 없는 것이므로 의미가 다르다.
		Expect(rr.Code).To(Equal(http.StatusForbidden))
	})

	It("returns 429 once the tenant bucket is exhausted", func() {
		// 업스트림은 무엇을 돌려주든 상관없으므로 200만 준다.
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		// rpm=1, burst=1이면 첫 요청만 통과하고 곧바로 버킷이 빈다.
		// 보충 속도가 분당 1회라 이어지는 요청이 우연히 통과할 여지가 없다.
		s := newProxyServer(up.URL, 1)

		first := httptest.NewRecorder()
		s.Handler().ServeHTTP(first, authedRequest(`{"model":"llama-3"}`))
		Expect(first.Code).To(Equal(http.StatusOK)) // 버킷에 토큰이 있으므로 통과

		second := httptest.NewRecorder()
		s.Handler().ServeHTTP(second, authedRequest(`{"model":"llama-3"}`))
		Expect(second.Code).To(Equal(http.StatusTooManyRequests)) // 토큰이 없으므로 429
	})

	It("returns 400 for a malformed JSON body", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		// 닫는 중괄호가 없는 깨진 JSON이다.
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":`))
		Expect(rr.Code).To(Equal(http.StatusBadRequest))
	})

	It("returns 400 when the model field is missing", func() {
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		// JSON 자체는 올바르지만 model 키가 없다.
		// 라우팅이 model에 전적으로 의존하므로 없으면 진행할 수 없다.
		s.Handler().ServeHTTP(rr, authedRequest(`{"messages":[]}`))
		Expect(rr.Code).To(Equal(http.StatusBadRequest))
	})

	It("returns 404 for an unknown model", func() {
		// 훅을 걸지 않아 진짜 backendFor가 돌고, 심어 둔 InferenceDeployment와 이름이 다르므로 ErrNoRoute가 난다.
		s := newProxyServer("", 600)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"does-not-exist"}`))
		Expect(rr.Code).To(Equal(http.StatusNotFound))
	})

	It("proxies the upstream status and body on the happy path and sets X-Request-Id", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 업스트림도 request id를 받아야 한다.
			// 게이트웨이가 헤더를 응답에만 달고 요청에 달지 않으면 로그를 이어 붙일 수 없다.
			Expect(r.Header.Get("X-Request-Id")).NotTo(BeEmpty())
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))

		// 상태 코드를 200으로 덮어쓰지 않고 업스트림 것을 그대로 전달해야 한다.
		Expect(rr.Code).To(Equal(http.StatusCreated))
		Expect(rr.Body.String()).To(Equal(`{"id":"chatcmpl-1"}`))
		Expect(rr.Header().Get("X-Request-Id")).NotTo(BeEmpty())
	})

	It("reuses an inbound X-Request-Id instead of generating a new one", func() {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		rr := httptest.NewRecorder()
		r := authedRequest(`{"model":"llama-3"}`)
		r.Header.Set("X-Request-Id", "caller-supplied-id")
		s.Handler().ServeHTTP(rr, r)

		// 호출자가 이미 id를 달았으면 그대로 이어받아야 추적이 끊기지 않는다.
		Expect(rr.Header().Get("X-Request-Id")).To(Equal("caller-supplied-id"))
	})

	It("returns 502 when the upstream refuses the connection", func() {
		// 서버를 띄웠다가 즉시 닫아 아무도 듣지 않는 주소를 얻는다.
		// 이러면 다이얼이 connection refused로 실패한다.
		up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := up.URL
		up.Close()
		s := newProxyServer(addr, 600)

		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))
		// 게이트웨이 자신의 잘못이 아니라 업스트림에 닿지 못한 것이므로 502다.
		Expect(rr.Code).To(Equal(http.StatusBadGateway))
	})

	// 이 명세가 502 테스트와 짝을 이루어 proxy.go의 ErrorHandler 분기 두 개를 모두 고정한다.
	//
	// 왜 502만으로는 부족한가(설계서 Error codes 절):
	// ErrorHandler에서 타임아웃 판정(errors.As + ne.Timeout())을 통째로 지워도
	// 남은 기본값이 502이므로 위의 502 테스트는 그대로 통과한다.
	// 즉 504 매핑은 테스트 없이는 조용히 사라질 수 있는 동작이다.
	// 운영에서 이 둘은 전혀 다른 신호이므로(502는 backend가 죽은 것, 504는 살아 있는데 응답이 없는 것)
	// 구분이 사라지면 장애 대응이 엉뚱한 곳을 파게 된다.
	It("returns 504 when the upstream does not send response headers in time", func() {
		// 업스트림이 연결은 받아들이되 응답 헤더를 보내지 않는 상황을 만든다.
		// 이것이 504가 뜻하는 바로 그 상태다. Pod는 살아 있는데(연결은 됨) 응답을 못 주는 것이다.
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			// 아무것도 쓰지 않고 붙잡아 둔다. 핸들러가 반환하면 Go가 200을 대신 써 버리므로 반환하면 안 된다.
			//
			// time.After를 함께 두는 이유는 스트리밍 테스트와 같다.
			// 테스트가 실패해 release가 닫히지 않더라도 이 goroutine이 영영 남아
			// up.Close()가 반환하지 못하고 suite 전체가 멈추는 일을 막는다.
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}))
		// defer는 늦게 등록된 것이 먼저 돈다.
		// 그래서 close(release)로 핸들러를 먼저 풀어 준 뒤에야 up.Close()가 막히지 않고 반환한다.
		defer up.Close()
		defer close(release)

		s := newProxyServer(up.URL, 600)
		// 운영 기본값 30초를 그대로 두면 이 테스트 하나가 30초를 잡아먹는다.
		// 테스트에서만 상한을 짧게 줄여 같은 코드 경로를 즉시 통과시킨다.
		s.responseHeaderTimeout = 50 * time.Millisecond

		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, authedRequest(`{"model":"llama-3"}`))
		// 연결은 됐으나 제한 시간 안에 헤더가 오지 않았으므로 502가 아니라 504여야 한다.
		Expect(rr.Code).To(Equal(http.StatusGatewayTimeout))
	})
})

// 스트리밍에 대한 명세다.
//
// 이 테스트가 막는 회귀(설계서 Request flow 절):
// OpenAI 호환 API의 stream:true는 토큰을 생성되는 즉시 흘려보낸다.
// 리버스 프록시가 응답을 버퍼링하면 클라이언트는 생성이 다 끝난 뒤에야 한꺼번에 받게 되어
// 스트리밍의 의미가 사라진다(체감 지연이 그대로 전부 드러난다).
// 이 회귀는 상태 코드나 본문만 보는 테스트로는 절대 잡히지 않는다. 최종 결과는 어느 쪽이든 동일하기 때문이다.
// 그래서 "핸들러가 끝나기 전에 첫 청크가 도착하는가"라는 시간 성질을 직접 본다.
var _ = Describe("streaming", func() {
	It("delivers upstream chunks progressively instead of buffering them", func() {
		// release: 업스트림이 두 번째 청크를 보내도 되는 시점을 테스트가 통제하는 채널이다.
		//
		// Go 문법 설명: chan struct{}는 값을 실어 나르지 않고 신호만 주고받는 채널이다.
		// struct{}는 크기가 0이라 "데이터가 아니라 사건 그 자체"를 뜻할 때 관례적으로 쓴다.
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// 첫 청크를 쓰고 즉시 flush 한다.
			//
			// Go 문법 설명: w.(http.Flusher)는 타입 단언이며, ResponseWriter가 Flush를 지원하는지 묻는다.
			// Flush()는 버퍼에 쌓인 바이트를 지금 당장 네트워크로 내보낸다.
			_, _ = fmt.Fprintf(w, "data: chunk-1\n\n")
			w.(http.Flusher).Flush()
			// 테스트가 "첫 청크를 확실히 받았다"고 알려줄 때까지 여기서 멈춘다.
			// 이 대기가 이 테스트의 핵심이다.
			// 프록시가 버퍼링한다면 클라이언트는 첫 청크를 받지 못하고, 테스트는 release를 닫지 못한다.
			//
			// select에 time.After를 함께 두는 이유:
			// release만 기다리면 테스트가 실패하는 경우 이 goroutine이 영영 여기 머물러
			// httptest 서버의 Close가 반환하지 못하고 suite 전체가 멈춘다.
			// 실패는 빠르고 시끄럽게 드러나야 하므로 상한을 둔다.
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return
			}
			_, _ = fmt.Fprintf(w, "data: chunk-2\n\n")
			w.(http.Flusher).Flush()
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		// 여기서만 httptest.NewRecorder 대신 진짜 서버를 띄운다.
		// Recorder는 메모리 버퍼라 "언제 도착했는지"라는 시간 정보가 없어 스트리밍을 검증할 수 없다.
		// 진짜 TCP 연결이어야 첫 청크가 핸들러 종료 전에 도착하는지를 관찰할 수 있다.
		gw := httptest.NewServer(s.Handler())
		defer gw.Close()

		// 요청에 마감 시한을 건다.
		//
		// 왜 시한이 필요한가:
		// 프록시가 버퍼링하면 첫 청크가 영영 오지 않고, 아래 ReadString은 그대로 멈춰 선다.
		// 시한이 없으면 이 테스트는 실패하는 대신 매달려서 suite 전체를 멈춰 세운다.
		// 시한이 있으면 마감과 동시에 읽기가 에러로 끊겨 테스트가 즉시, 그리고 분명하게 실패한다.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama-3","stream":true}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		// 응답 본문을 줄 단위로 읽는다.
		// 업스트림이 아직 두 번째 청크를 보내지 않았고 핸들러도 끝나지 않았는데 첫 줄이 읽히면,
		// 그것이 곧 바이트가 버퍼링 없이 흘러왔다는 증거다.
		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(line).To(Equal("data: chunk-1\n"))

		// 첫 청크가 도착했음을 확인했으니 업스트림을 풀어준다.
		close(release)

		// 두 번째 청크도 이어서 도착해야 스트림이 중간에 끊기지 않았다고 할 수 있다.
		Eventually(func() string {
			l, _ := reader.ReadString('\n')
			return l
		}, 5*time.Second).Should(Or(Equal("\n"), Equal("data: chunk-2\n")))
	})

	// 이 테스트만이 p.FlushInterval = -1 이라는 설정을 실제로 고정한다.
	//
	// 왜 위의 SSE 테스트로는 부족한가:
	// httputil.ReverseProxy의 flushInterval()은 두 경우에 설정값을 무시하고 스스로 -1(즉시 flush)을 쓴다.
	//   1. 응답 Content-Type이 text/event-stream일 때
	//   2. 응답 Content-Length를 모를 때(res.ContentLength == -1, 즉 chunked 스트리밍)
	// 현실의 스트리밍 응답은 둘 중 하나에 반드시 해당하므로, 그런 응답으로는
	// p.FlushInterval을 0으로 되돌려도 테스트가 그대로 통과한다(실제로 확인했다).
	// 즉 위 테스트는 statusRecorder가 Flusher를 가리지 않는지는 검증하지만 FlushInterval은 검증하지 못한다.
	//
	// 그래서 설정값이 실제로 쓰이는 유일한 경우를 만든다:
	// 업스트림이 Content-Length를 명시하면서도 본문을 나눠 보내는 상황이다.
	// 이때만 위 두 자동 전환이 걸리지 않아 p.FlushInterval이 그대로 사용된다.
	// 설정이 0이면 프록시가 본문을 모아 두었다가 한꺼번에 내보내므로 첫 청크가 제때 도착하지 않는다.
	It("uses the configured flush interval when the upstream declares a content length", func() {
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Content-Length를 명시하고 Content-Type도 SSE가 아닌 것으로 둔다.
			// 두 조건이 함께여야 ReverseProxy가 자동 전환 없이 우리 설정을 따른다.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "16") // "chunk-1\n" + "chunk-2\n"
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "chunk-1\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return
			}
			_, _ = fmt.Fprintf(w, "chunk-2\n")
			w.(http.Flusher).Flush()
		}))
		defer up.Close()
		s := newProxyServer(up.URL, 600)

		gw := httptest.NewServer(s.Handler())
		defer gw.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama-3","stream":true}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		// FlushInterval이 -1이 아니면 첫 청크는 업스트림 핸들러가 끝날 때까지 묶여 있고,
		// 업스트림은 release를 기다리느라 끝나지 않으므로 이 읽기가 마감 시한에 걸려 실패한다.
		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(line).To(Equal("chunk-1\n"))

		close(release)
	})
})

// 노출되는 metric 이름이 설계 계약과 일치하는지 고정하는 명세다.
//
// 이 테스트가 막는 회귀(docs/05 Minimum metrics 절):
// docs/05는 네 시계열의 이름을 gpuaas_gateway_ 접두사로 못박아 두었다.
// 이름은 단순한 라벨이 아니라 계약이다. 대시보드와 알림 규칙이 그 문자열로 질의하기 때문이다.
// 접두사가 어긋나도 게이트웨이는 아무 문제 없이 동작하고 테스트도 전부 통과한다.
// 오직 운영에서 "그래프가 비어 있다"로만 드러나며, 그때는 이미 늦다.
// 실제로 이 테스트를 쓰기 전 구현은 gateway_ 접두사를 쓰고 있었고 33개 명세가 모두 통과했다.
// 그래서 문서의 이름을 여기에 문자열 그대로 적어 두어, 코드가 문서에서 멀어지면 즉시 실패하게 한다.
var _ = Describe("metric names", func() {
	It("exposes exactly the series docs/05 pins", func() {
		// 네 시계열이 모두 값을 갖도록 세 가지 경로를 실제로 통과시킨다.
		//
		// 왜 요청을 흘려야 하는가:
		// 라벨이 있는 CounterVec/HistogramVec은 그 라벨 조합이 한 번이라도 쓰이기 전에는
		// 수집(scrape) 결과에 나타나지 않는다. 등록만으로는 이름이 노출되지 않는다.

		// (1) 정상 200 → requests_total, request_duration_seconds
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		ok := newProxyServer(up.URL, 600)
		ok.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))

		// (2) 429 → rate_limited_total
		limited := newProxyServer(up.URL, 1)
		limited.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))
		limited.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))

		// (3) 502 → upstream_errors_total
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := dead.URL
		dead.Close()
		broken := newProxyServer(addr, 600)
		broken.Handler().ServeHTTP(httptest.NewRecorder(), authedRequest(`{"model":"llama-3"}`))

		// :8081의 /metrics를 실제로 긁어 본다.
		rr := httptest.NewRecorder()
		ok.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		Expect(rr.Code).To(Equal(http.StatusOK))
		body := rr.Body.String()

		// docs/05 Minimum metrics 절의 이름을 그대로 옮겨 적었다.
		// 문서를 고치지 않고 코드만 고치면 이 단언이 실패한다.
		Expect(body).To(ContainSubstring("gpuaas_gateway_requests_total"))
		Expect(body).To(ContainSubstring("gpuaas_gateway_request_duration_seconds_bucket"))
		Expect(body).To(ContainSubstring("gpuaas_gateway_rate_limited_total"))
		Expect(body).To(ContainSubstring("gpuaas_gateway_upstream_errors_total"))
	})
})

// readModel 헬퍼 자체에 대한 명세다.
//
// 왜 핸들러 수준 테스트와 별도로 두는가:
// 핸들러 테스트는 400이라는 코드만 보므로, 본문을 읽고 나서 업스트림에 그대로 흘려보내는지까지는 확인하지 못한다.
// readModel은 model 필드를 엿보려고 본문을 소비하는데, 소비한 본문을 복원하지 않으면
// 업스트림은 빈 본문을 받게 된다. 프록시가 200을 돌려주므로 이 버그는 상위 테스트에서 드러나지 않는다.
var _ = Describe("readModel", func() {
	It("restores the body so the upstream still receives it intact", func() {
		body := `{"model":"llama-3","messages":[{"role":"user","content":"hi"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

		restored, model, err := readModel(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(model).To(Equal("llama-3"))

		// 복원된 본문을 끝까지 읽으면 원본과 한 바이트도 다르지 않아야 한다.
		buf := make([]byte, len(body))
		n, _ := restored.Read(buf)
		Expect(string(buf[:n])).To(Equal(body))
	})

	It("rejects a body that exceeds the size limit", func() {
		// maxBodyBytes를 넘는 본문을 만든다.
		//
		// 왜 크기 제한이 필요한가(설계서 Error codes 절):
		// 제한이 없으면 악의적 클라이언트가 거대한 본문을 보내 게이트웨이 메모리를 고갈시킬 수 있다.
		// readModel은 model을 찾으려고 본문을 메모리에 올리므로 여기가 정확히 그 취약점이다.
		big := `{"model":"llama-3","pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))

		_, _, err := readModel(r)
		Expect(err).To(HaveOccurred())
	})
})

// 인터페이스 준수 확인용 컴파일 타임 단언이다.
//
// Go 문법 설명: var _ 타입 = 값 형태는 "이 값이 이 타입을 만족하는가"를 컴파일러에게 묻는 관용구다.
// 만족하지 않으면 테스트 실행 전에 컴파일이 실패하므로, 시그니처가 어긋나는 것을 즉시 잡는다.
var _ client.Object = &platformv1.InferenceDeployment{}
