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

package gateway

import (
	// bytes: 소비한 요청 본문을 다시 읽을 수 있는 Reader로 되살릴 때 쓴다.
	"bytes"
	// crypto/rand: 추측 불가능한 난수를 만드는 암호학적 난수원이다.
	"crypto/rand"
	// encoding/hex: 난수 바이트를 사람이 읽고 로그에 붙일 수 있는 16진 문자열로 바꾼다.
	"encoding/hex"
	// encoding/json: 요청 본문에서 model 필드만 꺼낼 때 쓴다.
	"encoding/json"
	// errors: 아래 ErrNoModel 같은 감시 대상 에러(sentinel error)를 만든다.
	"errors"
	// io: 복원한 본문을 ReadCloser로 감쌀 때 쓴다.
	"io"
	// net: 타임아웃 여부를 구분하기 위해 net.Error 인터페이스를 본다.
	"net"
	// net/http: 프록시의 Transport와 상태 코드 상수가 여기서 온다.
	"net/http"
	// net/http/httputil: 표준 리버스 프록시 구현이다.
	"net/http/httputil"
	// net/url: 프록시의 대상 주소 타입이다.
	"net/url"
	// strconv: 상태 코드(int)를 metric 라벨(string)로 바꾼다.
	"strconv"
	// time: Transport의 각종 타임아웃 값에 쓴다.
	"time"
)

// maxBodyBytes: 요청 본문에서 읽어들일 최대 바이트 수다.
//
// 설계 근거(설계서 Error codes 절): readModel은 model 필드를 보려고 본문을 메모리에 올린다.
//
// 제한이 없으면 악의적 클라이언트가 수 GB짜리 본문을 흘려보내 게이트웨이 메모리를 고갈시킬 수 있다.
//
// 1MB는 정상적인 chat completions 요청(수십 KB 수준)보다 넉넉히 크면서도 한 요청이 프로세스를 위협할 수 없는 크기다.
const maxBodyBytes = 1 << 20 // 1MB

// ErrNoModel: 본문에 model 필드가 없거나 비어 있을 때 반환한다.
//
// Go 문법 설명: ratelimit.go의 ErrNoPolicy, router.go의 ErrNoRoute와 같은 방식이다.
//
// 전역에 값 하나를 만들어 두면 호출한 쪽이 errors.Is로 정확히 이 경우만 골라낼 수 있다.
var ErrNoModel = errors.New("request body has no model field")

// newRequestID: 요청 하나를 식별하는 임의의 16진 문자열을 만든다.
//
// 왜 crypto/rand인가.
//
// math/rand는 시드가 같으면 같은 수열이 나오는 예측 가능한 난수라, 서로 다른 Pod가 같은 id를 내놓을 수 있다.
//
// id가 겹치면 로그를 이어 붙일 때 서로 다른 요청이 한 요청으로 뭉쳐 추적이 무의미해진다.
//
// crypto/rand는 OS의 엔트로피를 쓰므로 그런 충돌이 사실상 일어나지 않는다.
//
// Go 문법 설명: rand.Read는 준 슬라이스를 난수로 채운다.
//
// crypto/rand.Read는 실패하면 에러를 주지만 현대 OS에서 사실상 실패하지 않는다.
//
// 그래도 에러를 무시하지 않고, 실패 시 빈 문자열 대신 시각 기반 대체값을 쓰도록 했다.
//
// id가 없는 것보다는 덜 이상적인 id라도 있는 편이 추적에 낫기 때문이다.
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// readModel: 요청 본문에서 model 이름만 꺼내고, 소비한 본문을 그대로 복원해 돌려준다.
//
// 반환값 (io.ReadCloser, string, error): 복원된 본문, model 이름, 에러 순이다.
//
// 왜 본문을 복원해야 하는가.
//
// r.Body는 한 번만 읽을 수 있는 스트림이다.
//
// 여기서 model을 보려고 끝까지 읽어 버리면 뒤이어 프록시가 업스트림으로 보낼 때는 이미 다 읽힌 빈 스트림만 남는다.
//
// 그러면 모델 서버는 빈 본문을 받아 400을 돌려주는데, 게이트웨이는 그것을 그대로 전달할 뿐이라 원인이 게이트웨이에 있다는 사실이 드러나지 않는다.
//
// 그래서 읽은 바이트를 bytes.NewReader로 되감아 새 ReadCloser로 만들어 돌려준다.
//
// Go 문법 설명.
//
//   - http.MaxBytesReader(w, body, n)는 n바이트를 넘으면 읽기가 에러가 되는 Reader를 씌운다.
//     첫 인자로 nil을 넘긴 것은 응답을 여기서 직접 쓰지 않기 때문이다(상태 코드는 호출한 쪽이 정한다).
//   - io.ReadAll은 Reader를 끝까지 읽어 바이트 슬라이스로 만든다.
//   - io.NopCloser는 Close가 아무것도 하지 않는 ReadCloser로 감싼다.
//     bytes.Reader에는 Close가 없는데 r.Body는 ReadCloser여야 하므로 이 감싸기가 필요하다.
func readModel(r *http.Request) (io.ReadCloser, string, error) {
	// 크기 제한을 씌운 뒤 본문을 통째로 읽는다.
	//
	// 제한을 넘으면 여기서 에러가 나고, 호출한 쪽이 400으로 바꾼다.
	limited := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}

	// 본문 전체를 구조체로 풀지 않고 model 필드 하나만 꺼낸다.
	//
	// Go 문법 설명: struct 태그 `json:"model"`은 JSON의 model 키를 이 필드에 대응시킨다.
	//
	// 익명 구조체(변수 선언과 동시에 정의하는 타입)를 쓴 이유는 이 함수 밖에서 쓸 일이 없기 때문이다.
	//
	// 왜 나머지 필드를 무시하는가.
	//
	// 게이트웨이는 라우팅에 필요한 model만 알면 되고, messages나 temperature 같은 나머지는 해석하지 않고 그대로 흘려보내야 한다.
	//
	// 전체를 구조체로 정의하면 OpenAI API에 필드가 추가될 때마다 게이트웨이를 고쳐야 하고, 정의하지 않은 필드가 조용히 사라져 업스트림에 전달되지 않는 사고가 난다.
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(buf, &probe); err != nil {
		return nil, "", err
	}
	if probe.Model == "" {
		return nil, "", ErrNoModel
	}

	// 읽어 둔 바이트로 본문을 되살린다.
	//
	// 이 Reader는 방금 읽은 것과 정확히 같은 바이트를 처음부터 다시 내놓는다.
	return io.NopCloser(bytes.NewReader(buf)), probe.Model, nil
}

// statusRecorder: 프록시가 실제로 쓴 상태 코드를 가로채 기록하는 ResponseWriter 래퍼다.
//
// 왜 필요한가.
//
// 응답을 다 보낸 뒤 metric에 "어떤 코드로 끝났는지"를 남겨야 하는데, http.ResponseWriter에는 "방금 무슨 코드를 썼는지" 되묻는 방법이 없다.
//
// 그래서 중간에 끼어들어 WriteHeader 호출을 엿보는 래퍼를 둔다.
//
// Go 문법 설명.
//
//   - 구조체 안에 타입 이름만 적는 것(http.ResponseWriter)은 "임베딩(embedding)"이다.
//     임베딩하면 그 인터페이스의 메서드들이 자동으로 이 구조체의 메서드가 된다.
//     덕분에 Write나 Header를 일일이 다시 구현하지 않아도 되고, 우리가 관심 있는 WriteHeader만 덮어쓰면 된다.
//   - 이 래퍼는 Flush를 직접 구현하지 않으므로 아래 별도 처리가 필요하다(주석 참고).
type statusRecorder struct {
	http.ResponseWriter
	// code: 기록된 상태 코드다.
	//
	// 200으로 초기화하는 이유는 핸들러가 WriteHeader를 부르지 않고 바로 Write만 하면 Go가 암묵적으로 200을 보내기 때문이다.
	//
	// 그 경우에도 metric에 200이 남아야 맞다.
	code int
}

// WriteHeader: 상태 코드를 기록한 뒤 원래 ResponseWriter로 그대로 넘긴다.
//
// Go 문법 설명: rec.ResponseWriter.WriteHeader(c)처럼 임베딩된 필드를 명시해 호출하면 우리가 덮어쓴 메서드가 아니라 원본의 메서드가 불린다.
//
// 이 줄이 없으면 상태 코드가 기록만 되고 클라이언트에는 전달되지 않는다.
func (rec *statusRecorder) WriteHeader(c int) {
	rec.code = c
	rec.ResponseWriter.WriteHeader(c)
}

// Flush: 버퍼에 쌓인 바이트를 즉시 내보낸다.
//
// 왜 이 메서드가 반드시 있어야 하는가(설계서 Request flow 절).
//
// 임베딩은 인터페이스에 선언된 메서드만 물려준다.
//
// http.ResponseWriter에는 Flush가 없으므로, 이 래퍼는 http.Flusher를 만족하지 않게 된다.
//
// 리버스 프록시는 스트리밍을 하기 전에 응답 writer가 Flusher인지 확인하는데, 아니라고 판단하면 FlushInterval 설정과 무관하게 응답을 버퍼링해 버린다.
//
// 즉 이 메서드가 없으면 statusRecorder를 끼운 것만으로 스트리밍이 조용히 죽는다.
//
// Go 문법 설명: 타입 단언에 값 두 개를 받는 형태(v, ok := x.(T))는 실패해도 패닉이 나지 않고 ok가 false가 된다.
//
// 원본 writer가 Flush를 지원할 때만 위임하고, 아니면 아무것도 하지 않는다.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// defaultResponseHeaderTimeout: 업스트림 응답 헤더를 기다리는 운영 기본 상한이다.
//
// 왜 30초인가.
//
// 모델 서버는 첫 토큰을 내놓기까지(prefill) 수 초가 걸릴 수 있으므로 짧게 잡으면 정상 요청이 잘린다.
//
// 반대로 무한정 기다리면 매달린 요청이 쌓여 게이트웨이의 연결이 고갈된다.
//
// 그 사이의 타협점이다.
const defaultResponseHeaderTimeout = 30 * time.Second

// newReverseProxy: target으로 요청을 넘기는 스트리밍 가능한 리버스 프록시를 만든다.
//
// 업스트림에 닿지 못하면 onErr로 상태 코드를 알리고 502/504로 응답한다.
//
// Go 문법 설명.
//
//   - 인자 onErr func(code int)는 "함수를 인자로 받는다"는 뜻이다.
//     프록시 안에서 벌어진 일을 바깥(metric 기록, 상태 코드 갱신)에 알리는 콜백이다.
//   - httputil.NewSingleHostReverseProxy(u)는 모든 요청을 u 한 곳으로 넘기는 프록시를 만든다.
//
// responseHeaderTimeout이 0이면 위의 기본값을 쓴다.
//
// 0을 "설정하지 않음"으로 해석하는 이유는, 0을 그대로 Transport에 넘기면 http.Transport가 그것을 "상한 없음(무한 대기)"으로 읽어 우리가 막으려던 바로 그 상황이 되기 때문이다.
func newReverseProxy(target *url.URL, responseHeaderTimeout time.Duration, onErr func(code int)) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(target)
	if responseHeaderTimeout == 0 {
		responseHeaderTimeout = defaultResponseHeaderTimeout
	}

	// FlushInterval = -1: 받는 즉시 클라이언트로 흘려보낸다("버퍼링하지 말라"는 뜻의 특별한 값이다).
	//
	// 설계 근거(설계서 Request flow 절): OpenAI 호환 API의 stream:true는 토큰을 하나씩 흘려보낸다.
	//
	// 프록시가 응답을 모아 두었다가 한꺼번에 내보내면 클라이언트는 생성이 끝난 뒤에야 전부 받게 되어 스트리밍이 무의미해진다.
	//
	// 다만 이 한 줄이 실제로 쓰이는 범위는 좁다.
	//
	// 정확히 알아 두는 편이 낫다.
	//
	// httputil.ReverseProxy의 flushInterval()은 아래 두 경우 이 설정을 무시하고 스스로 즉시 flush를 택한다.
	//
	//   1. 응답 Content-Type이 text/event-stream일 때
	//   2. 응답 Content-Length를 모를 때(chunked 스트리밍)
	//
	// 현실의 스트리밍 응답은 대부분 둘 중 하나에 해당하므로, 그런 경우엔 이 설정이 없어도 결과가 같다.
	//
	// 이 설정이 결과를 바꾸는 경우는 "업스트림이 Content-Length를 명시하면서 본문을 나눠 보내는" 상황뿐이다.
	//
	// 그 상황을 proxy_test.go의 "uses the configured flush interval..." 명세가 고정하고 있다.
	//
	// 그런 경우가 드문데도 남겨 두는 이유는, Go의 자동 판정에 기대는 것이 우리 의도를 표현하지 않기 때문이다.
	//
	// 이 게이트웨이는 어떤 응답이든 버퍼링하지 않겠다는 것이 설계 의도이며, 그 의도는 코드에 드러나 있어야 한다.
	p.FlushInterval = -1

	// Transport: 업스트림으로 나가는 HTTP 연결의 동작을 정한다.
	//
	// 왜 기본 Transport를 그대로 쓰지 않는가(설계서 Request flow 절).
	//
	// http.DefaultTransport에는 응답 헤더 대기 타임아웃이 없다.
	//
	// 모델 서버가 연결만 받아 두고 영영 응답하지 않으면 그 요청은 무한정 매달려 있고, 그런 요청이 쌓이면 게이트웨이의 연결과 메모리가 고갈된다.
	//
	// 왜 전체 요청 타임아웃(Timeout)은 걸지 않는가.
	//
	// 스트리밍 응답은 정상적으로도 수 분씩 이어질 수 있다.
	//
	// 전체 시간에 상한을 두면 정상적인 긴 스트림이 중간에 끊긴다.
	//
	// 그래서 "첫 응답 헤더까지"만 제한하고, 그 이후 본문이 흐르는 시간은 제한하지 않는다.
	p.Transport = &http.Transport{
		// ResponseHeaderTimeout: 요청을 다 보낸 뒤 응답 헤더가 오기까지 기다리는 최대 시간이다.
		//
		// 이 시간을 넘기면 타임아웃 에러가 되어 아래 ErrorHandler가 504로 바꾼다.
		ResponseHeaderTimeout: responseHeaderTimeout,
		// IdleConnTimeout: 재사용을 위해 열어 둔 유휴 연결을 얼마나 유지할지다.
		//
		// 연결을 재사용하면 요청마다 TCP 핸드셰이크를 다시 하지 않아 지연이 줄어든다.
		IdleConnTimeout: 90 * time.Second,
	}

	// ErrorHandler: 업스트림에 닿지 못했을 때 불린다.
	//
	// 왜 기본 동작을 덮어쓰는가(설계서 Error codes 절).
	//
	// ReverseProxy의 기본 ErrorHandler는 무조건 502를 쓰고 에러를 로그로만 남긴다.
	//
	// 그러면 "연결이 거부됨(502)"과 "응답이 없어 시간 초과(504)"가 구분되지 않는다.
	//
	// 이 둘은 운영상 전혀 다른 신호다.
	//
	// 502는 backend Pod가 죽었거나 Service가 잘못 연결된 것이고, 504는 Pod는 살아 있는데 너무 느리거나 매달린 것이다.
	//
	// 대응이 다르므로 코드로 구분해 준다.
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// 기본은 502다.
		//
		// 연결 거부, DNS 실패 등이 여기 해당한다.
		code := http.StatusBadGateway
		// 타임아웃이면 504로 바꾼다.
		//
		// Go 문법 설명: errors.As는 에러 체인을 따라가며 그 안에 net.Error가 있는지 찾는다.
		//
		// 단순 타입 단언(err.(net.Error))을 쓰면 %w로 감싸인 에러를 놓치므로 errors.As가 맞다.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			code = http.StatusGatewayTimeout
		}
		// 바깥에 알린다(metric 증가 + 기록될 상태 코드 갱신).
		onErr(code)
		// 클라이언트에게 상태 코드와 사유를 돌려준다.
		http.Error(w, err.Error(), code)
	}

	return p
}
