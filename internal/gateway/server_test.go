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

// package 선언: 테스트 파일도 테스트 대상과 같은 gateway 패키지에 속한다.
//
// Go 문법 설명.
//
//   - 파일 이름이 _test.go로 끝나면 Go는 이 파일을 테스트 전용으로 취급한다.
//     즉 go build로 만드는 실제 binary에는 포함되지 않고, go test를 돌릴 때만 컴파일된다.
//   - 패키지 이름을 gateway_test가 아니라 gateway로 둔 것은 "내부 테스트"라는 뜻이다.
//     덕분에 소문자로 감춰진 readyz나 markReady 같은 비공개 멤버를 직접 호출할 수 있다.
package gateway

// import 블록: 이 테스트가 사용하는 패키지들이다.
import (
	// net/http: 요청 메서드 상수(MethodGet)와 상태 코드 상수(StatusOK 등)를 쓰기 위해 필요하다.
	"net/http"
	// net/http/httptest: 진짜 서버를 띄우거나 포트를 열지 않고 HTTP handler를 테스트하는 표준 도구다.
	"net/http/httptest"
	// testing: Go의 표준 테스트 프레임워크이며, 아래 *testing.T 타입이 여기서 온다.
	"testing"

	// ginkgo/gomega: BDD 스타일 테스트 프레임워크(ginkgo)와 단언 라이브러리(gomega)다.
	//
	// Go 문법 설명: import 경로 앞의 점(.)은 "dot import"라는 특수한 별칭이다.
	//
	// 보통은 ginkgo.Describe처럼 패키지 이름을 붙여야 하지만, 점을 쓰면 Describe/It/Expect를 접두사 없이 바로 쓸 수 있다.
	//
	// 일반 코드에서는 이름 출처가 흐려져 권장되지 않지만, 테스트에서는 명세를 문장처럼 읽히게 하려고 관례적으로 허용한다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestGateway: gateway package의 Ginkgo suite를 표준 go test에 등록하는 진입점이다.
//
// Go 문법 설명.
//
//   - go test는 "Test로 시작하고 *testing.T 하나를 받는 함수"만 테스트로 인식해 실행한다.
//     그래서 Ginkgo를 쓰더라도 이런 표준 함수 하나가 반드시 있어야 한다.
//   - 이 파일들의 Describe 블록들은 go test가 직접 아는 대상이 아니다.
//     이 함수가 RunSpecs를 부르는 순간에야 비로소 실행된다.
//   - 패키지당 이런 함수는 하나만 두는 것이 관례이며, 여러 개를 두면 같은 spec이 중복 실행된다.
//
// RegisterFailHandler(Fail): gomega의 단언(Expect)이 실패했을 때 무엇을 할지 알려주는 연결 작업이다.
//
// gomega는 자기 혼자서는 테스트를 실패시킬 방법을 모르고, ginkgo의 Fail 함수를 건네받아야 한다.
//
// 이 줄을 빠뜨리면 Expect가 실패해도 테스트가 그냥 통과한 것처럼 보인다.
//
// RunSpecs(t, "Gateway Suite"): 등록된 모든 Ginkgo spec을 실행하고 결과를 t에 보고한다.
//
// 두 번째 인자는 테스트 출력에 표시되는 suite 이름이다.
func TestGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Suite")
}

// readiness에 대한 명세 묶음이다.
//
// Go 문법 설명.
//
//   - Describe("설명", func() {...})는 관련된 테스트들을 하나로 묶는 Ginkgo의 컨테이너다.
//     그 안의 It("설명", func() {...})가 실제로 실행되는 테스트 하나다.
//   - var _ = Describe(...) 라는 형태가 낯설 수 있다.
//     Describe는 함수 호출이라 값을 돌려주는데, Go는 쓰지 않는 값을 패키지 수준에 그냥 둘 수 없다.
//     그래서 밑줄(_) 변수에 대입해 "결과는 버린다"고 알린다.
//     진짜 목적은 반환값이 아니라, 패키지 변수 초기화 시점에 Describe가 실행되며 spec이 Ginkgo에 등록되는 부수 효과다.
//   - Describe와 It에 들어가는 문자열은 주석이 아니라 실행되는 코드 리터럴이며, 테스트 출력에 그대로 찍힌다.
//
// 이 테스트가 막는 회귀(설계서 Components 절).
//
// cache 동기화 전에 /readyz가 200을 반환하면, 쿠버네티스가 아직 아무것도 모르는 Pod를 Service endpoint에 넣어버려 실사용 요청이 잘못된 오류를 받게 된다.
//
// 반대로 markReady 이후에도 계속 503이면 Pod가 영영 트래픽을 못 받아 배포가 멈춘다.
//
// 그래서 "전에는 503, 후에는 200"이라는 전환을 양쪽 다 검증한다.
var _ = Describe("readiness", func() {
	It("is 503 before cache sync and 200 after", func() {
		// &Server{} 는 모든 필드가 제로값인 Server를 만들어 그 포인터를 얻는다.
		//
		// ready 필드(atomic.Bool)의 제로값은 false이므로, 이 서버는 "아직 준비 안 됨" 상태로 출발한다.
		//
		// 이 테스트는 Client를 전혀 쓰지 않으므로 nil인 채로 둬도 문제가 없다.
		s := &Server{}
		// httptest.NewRecorder()는 http.ResponseWriter 인터페이스를 구현한 가짜 응답 기록기다.
		//
		// 네트워크로 내보내는 대신 상태 코드와 본문을 메모리에 담아두므로, 나중에 꺼내 검사할 수 있다.
		rr := httptest.NewRecorder()
		// handler를 HTTP 서버를 띄우지 않고 평범한 함수처럼 직접 호출한다.
		//
		// httptest.NewRequest(메서드, 경로, 본문)는 테스트용 가짜 요청을 만든다.
		//
		// 마지막 인자 nil은 "요청 본문 없음"을 뜻하며, GET /readyz엔 본문이 필요 없다.
		s.readyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		// rr.Code에는 handler가 쓴 상태 코드가 기록되어 있다.
		//
		// Expect(실제값).To(Equal(기대값))이 gomega의 기본 단언 형태다.
		Expect(rr.Code).To(Equal(http.StatusServiceUnavailable)) // cache 동기화 전이라 503
		// 이제 cache가 동기화된 상황을 흉내 낸다.
		//
		// 같은 패키지라서 소문자 markReady를 직접 부를 수 있다(공개 API인 MarkReady를 거칠 필요가 없다).
		s.markReady()
		// 기록기를 새로 만든다.
		//
		// Recorder는 상태 코드를 한 번만 기록하므로, rr을 재사용하면 두 번째 응답이 제대로 반영되지 않는다.
		rr2 := httptest.NewRecorder()
		s.readyz(rr2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		Expect(rr2.Code).To(Equal(http.StatusOK)) // markReady 이후 200
	})
})

// The point of admission_mode_active is that it is present when nothing else about admission is, so the two
// things worth asserting are that it appears for a mode which publishes no other series at all ("off"), and
// that switching modes leaves exactly one series behind rather than accumulating one per mode ever set — a
// reader seeing two modes at 1 has no way to tell which one is running.
var _ = Describe("admission mode series", func() {
	It("reports the installed mode and never leaves a stale one behind", func() {
		s := &Server{}

		s.SetAdmitter(AdmissionOff, nil)
		Expect(testutil.ToFloat64(admissionModeActive.WithLabelValues(string(AdmissionOff)))).To(Equal(1.0))
		Expect(testutil.CollectAndCount(admissionModeActive)).To(Equal(1))

		s.SetAdmitter(AdmissionKVAware, nil)
		Expect(testutil.ToFloat64(admissionModeActive.WithLabelValues(string(AdmissionKVAware)))).To(Equal(1.0))
		Expect(testutil.CollectAndCount(admissionModeActive)).To(Equal(1))
	})
})
