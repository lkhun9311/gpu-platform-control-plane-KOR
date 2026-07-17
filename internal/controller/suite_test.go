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
// 파일 이름이 _test.go로 끝나면 Go 도구는 이 파일을 테스트 전용으로 취급한다.
// 즉 go build로 만드는 실제 바이너리에는 절대 포함되지 않고 go test를 돌릴 때만 컴파일된다.
// package 이름이 controller_test가 아니라 controller인 점이 중요하다.
// 테스트 대상과 같은 패키지 안에 있어야 setPhase나 nodeHealthFinalizer 같은 소문자(비공개) 식별자에 접근할 수 있다.
//
// 설계 근거: 이 파일은 특정 테스트 하나가 아니라 이 패키지의 모든 컨트롤러 테스트가 공유하는 "무대 장치"를 담당한다.
// 테스트용 쿠버네티스 API 서버를 켜고 끄는 일을 파일마다 반복하지 않도록 한곳에 모아 둔 것이다.
package controller

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// context: 취소/타임아웃 신호를 함수 사이로 전달하는 표준 타입이다.
	"context"
	// os: 운영체제 기능을 쓰는 표준 패키지이며, 여기서는 os.ReadDir로 디렉터리를 읽는다.
	"os"
	// filepath: 파일 경로를 OS에 맞게 조립하는 표준 패키지다.
	// 경로를 "a/b" 처럼 문자열로 직접 이어 붙이지 않고 이 패키지를 쓰는 게 관례다.
	"path/filepath"
	// testing: Go의 표준 테스트 패키지이며, 아래 TestControllers의 *testing.T가 여기서 온다.
	"testing"
	// time: 시간과 기간을 다루는 표준 패키지이며, 아래 Eventually의 타임아웃 지정에 쓴다.
	"time"

	// ginkgo/gomega: BDD 스타일 테스트 프레임워크(ginkgo)와 단언 라이브러리(gomega)다.
	//
	// Go 문법 설명:
	//   - import 경로 앞의 점(.)은 "dot import"이며, 그 패키지의 공개 이름들을 패키지 이름 없이 바로 쓰게 해 준다.
	//   - 즉 ginkgo.Describe(...) 대신 Describe(...)라고 쓸 수 있다.
	//   - dot import는 이름 충돌 위험 때문에 보통은 피하지만, ginkgo/gomega만은 예외적으로 권장되는 관용이다.
	//     테스트가 영어 문장처럼 읽히게 하는 것이 이 프레임워크들의 설계 의도이기 때문이다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// scheme: client-go가 제공하는, 쿠버네티스 기본 타입들이 이미 등록된 전역 scheme이다.
	"k8s.io/client-go/kubernetes/scheme"
	// rest: API 서버 접속 정보(주소, 인증서 등)를 담는 rest.Config 타입을 제공한다.
	"k8s.io/client-go/rest"
	// client: controller-runtime의 쿠버네티스 클라이언트이며, 테스트에서 객체를 만들고 읽는 데 쓴다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// envtest: 진짜 kube-apiserver와 etcd 바이너리를 로컬 프로세스로 띄워 주는 테스트 도구다.
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	// logf: controller-runtime의 로깅 진입점이며, logf는 이 패키지에 붙인 별칭이다.
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	// zap: 위 로깅 인터페이스의 실제 구현체를 만들어 주는 어댑터다.
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(NodeHealth, GPUQuotaPolicy 등)이다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	// 아래 줄은 주석처럼 보이지만 kubebuilder가 코드를 자동 생성할 때 찾는 마커(marker)다.
	// 새 API를 추가하면 kubebuilder가 이 표식을 찾아 바로 그 자리에 import 줄을 끼워 넣는다.
	// 사람이 읽는 설명이 아니라 도구가 읽는 앵커이므로 번역하거나 지우면 안 된다.
	// +kubebuilder:scaffold:imports
)

// 이 테스트들은 Ginkgo(BDD 스타일 Go 테스트 framework)를 사용하며,
// Ginkgo 소개는 http://onsi.github.io/ginkgo/ 참고.
//
// 이 스위트의 핵심 도구는 envtest다.
// envtest는 실제 kube-apiserver와 etcd 실행 파일을 로컬에서 임시 프로세스로 띄우는 방식이다.
// 즉 가짜(mock) API 서버가 아니라 진짜 API 서버를 상대로 테스트가 돈다.
//
// 설계 근거: 왜 진짜 클러스터(kind, minikube 등) 대신 envtest를 쓰는가.
//   - 진짜 API 서버라서 CRD 스키마 검증, defaulting, optimistic concurrency 같은 실제 동작이 그대로 재현된다.
//     클라이언트를 mock으로 흉내 내면 이런 서버 측 동작을 전혀 검증할 수 없다.
//   - 반대로 kubelet, scheduler, controller-manager는 뜨지 않아 시작이 수 초 수준으로 빠르고 도커도 필요 없다.
//   - 부작용도 있다.
//     scheduler와 kubelet이 없으므로 pod을 만들어도 절대 Running이 되지 않는다.
//     그래서 node의 Ready condition 같은 상태는 테스트가 손으로 써 넣어 상황을 연출해야 한다.
//   - 매 실행마다 빈 etcd로 시작하므로 테스트끼리 상태가 새지 않는다.

// 이 패키지의 테스트들이 공유하는 전역 변수들이다.
//
// Go 문법 설명:
//   - var ( ... ) 처럼 괄호로 묶으면 변수 여러 개를 한 블록에 모아 선언할 수 있다.
//   - 초기값을 적지 않으면 각 타입의 제로값(포인터/인터페이스는 nil)으로 시작한다.
//   - 전역으로 두는 이유는 BeforeSuite가 채운 값을 다른 파일의 테스트들이 그대로 써야 하기 때문이다.
//     테스트는 한 번에 하나씩 순차 실행되므로 전역 공유로 인한 경합 걱정은 없다.
//
// 각 변수의 역할은 다음과 같다.
//   - ctx: 스위트 전체 수명을 대표하는 context이며, 테스트 안에서 클라이언트 호출에 넘긴다.
//   - cancel: 위 ctx를 취소하는 함수이며, AfterSuite에서 호출해 뒷정리를 알린다.
//   - testEnv: envtest가 띄운 임시 API 서버 환경 자체를 가리키며, Start/Stop의 주체다.
//   - cfg: 그 임시 API 서버에 접속하기 위한 정보(주소, 인증 정보)이며, testEnv.Start()가 돌려준다.
//   - k8sClient: 테스트가 객체를 Create/Get/Update할 때 쓰는 클라이언트다.
//
// cancel의 타입인 context.CancelFunc는 "인자도 반환값도 없는 함수"를 가리키는 타입이다.
// Go에서는 함수도 값이라서 이렇게 변수에 담아 두었다가 나중에 부를 수 있다.
var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

// TestControllers: Go 표준 테스트 러너와 Ginkgo를 이어 주는 유일한 다리다.
//
// Go 문법 설명:
//   - go test는 Test로 시작하고 *testing.T 하나를 받는 함수만 테스트로 인식해 실행한다.
//     Ginkgo의 Describe/It는 표준 러너가 알아보지 못하는 형식이라 이런 진입점이 반드시 하나 필요하다.
//   - t는 표준 테스트 컨텍스트이며, 실패 보고와 로그 출력의 창구다.
//
// 설계 근거: 이 함수가 파일당 하나가 아니라 패키지당 하나만 있으면 된다.
// RunSpecs가 이 패키지에 등록된 모든 Ginkgo 스펙을 한꺼번에 찾아 돌려주기 때문이다.
func TestControllers(t *testing.T) {
	// RegisterFailHandler: gomega의 단언이 실패했을 때 무엇을 부를지 알려 준다.
	// Ginkgo의 Fail 함수를 넘겨 두면 Expect(...) 실패가 곧바로 Ginkgo 스펙 실패로 이어진다.
	// Fail 뒤에 괄호가 없는 점에 주목하자.
	// 지금 호출하는 게 아니라 함수 자체를 값으로 넘겨 나중에 대신 부르게 하는 것이다.
	// 이 등록을 빠뜨리면 단언이 실패해도 테스트가 그냥 통과한 것처럼 보인다.
	RegisterFailHandler(Fail)

	// RunSpecs: 이 패키지에 등록된 모든 Ginkgo 스펙을 실행하고 결과를 t에 보고한다.
	// 두 번째 인자 "Controller Suite"는 리포트에 찍히는 스위트 이름이다.
	// 이것은 주석이 아니라 실행되는 문자열 리터럴이므로 번역 대상이 아니다.
	RunSpecs(t, "Controller Suite")
}

// BeforeSuite: 이 패키지의 첫 스펙이 돌기 전에 딱 한 번 실행되는 준비 훅이다.
// 여기서 임시 API 서버를 띄우고 클라이언트를 만든다.
//
// Go 문법 설명:
//   - var _ = BeforeSuite(func() { ... }) 는 Ginkgo 특유의 등록 관용구다.
//   - func() { ... } 는 이름 없는 함수(익명 함수)를 값으로 만든 것이며, 여기서 바로 실행되지 않고 Ginkgo에 등록만 된다.
//   - 밑줄(_)은 "이 값을 받긴 하지만 쓰지는 않겠다"는 뜻의 빈 식별자다.
//     Go는 쓰지 않는 변수를 컴파일 에러로 막지만 _ 는 예외다.
//   - 굳이 var 로 감싸는 이유는 Go에서 함수 호출을 최상위(top-level)에 그냥 쓸 수 없기 때문이다.
//     전역 변수의 초기화 식으로 만들면 패키지 로딩 시점에 BeforeSuite(...)가 실행되어 훅이 등록된다.
//
// 설계 근거: 스펙마다 API 서버를 새로 띄우면 매번 수 초가 들어 테스트가 못 쓸 만큼 느려진다.
// 그래서 서버는 스위트당 한 번만 띄우고, 테스트 간 격리는 스펙마다 다른 이름의 객체를 쓰는 식으로 확보한다.
var _ = BeforeSuite(func() {
	// controller-runtime의 전역 로거를 설정한다.
	// zap.WriteTo(GinkgoWriter): 로그를 Ginkgo의 출력 버퍼로 보낸다.
	// GinkgoWriter는 테스트가 실패했을 때만 내용을 실제로 뱉으므로, 성공한 테스트의 로그로 화면이 더럽혀지지 않는다.
	// zap.UseDevMode(true): 사람이 읽기 좋은 형식으로 출력한다(운영에서 쓰는 JSON 형식 대신).
	// 이 설정을 안 하면 controller-runtime이 "로거가 설정되지 않았다"는 경고를 계속 뱉는다.
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	// 스위트 전역 ctx와 그 취소 함수를 만든다.
	// context.WithCancel(부모)는 "취소 가능한 자식 context"와 "그걸 취소하는 함수" 둘을 돌려준다.
	// context.TODO()는 마땅한 부모 context가 없을 때 쓰는 빈 뿌리 context다.
	// = 를 쓴 이유는 ctx와 cancel이 이미 위에서 var로 선언된 전역 변수라서다.
	// 여기서 := 를 쓰면 같은 이름의 새 지역 변수가 만들어져 전역은 nil인 채로 남는 흔한 함정에 빠진다.
	ctx, cancel = context.WithCancel(context.TODO())

	// err 변수를 미리 선언해 둔다.
	// 아래에서 전역 변수(cfg, k8sClient)에 대입할 때 := 를 쓸 수 없으므로 err만 따로 준비하는 것이다.
	var err error
	// AddToScheme: 우리 CRD 타입들을 client-go의 전역 scheme에 등록한다.
	//
	// scheme이란 "Go 타입 ↔ 쿠버네티스 GroupVersionKind" 사이의 번역표다.
	// 클라이언트는 이 표를 보고 NodeHealth 구조체를 어떤 apiVersion/kind의 JSON으로 직렬화할지 판단한다.
	// 이 등록을 빠뜨리면 아래 client.New는 성공해도 실제 NodeHealth를 Create할 때
	// "no kind is registered for the type" 에러가 나며 실패한다.
	err = platformv1.AddToScheme(scheme.Scheme)
	// Expect(err).NotTo(HaveOccurred())는 gomega의 단언이며 "에러가 없어야 한다"는 뜻이다.
	// 준비 단계가 실패하면 이후 모든 스펙이 무의미하므로 여기서 즉시 스위트를 중단시킨다.
	Expect(err).NotTo(HaveOccurred())

	// 아래 줄도 kubebuilder가 읽는 마커다.
	// 새 API를 추가하면 kubebuilder가 이 자리에 AddToScheme 호출을 자동으로 끼워 넣는다.
	// 도구용 앵커이므로 번역하거나 지우면 안 된다.
	// +kubebuilder:scaffold:scheme

	// By(...)는 Ginkgo가 리포트에 남기는 진행 단계 표시다.
	// 테스트가 실패했을 때 "어느 단계까지 갔다가 죽었는지" 알려 주는 이정표 역할을 한다.
	// 이것도 주석이 아니라 실행되는 문자열 리터럴이므로 번역 대상이 아니다.
	By("bootstrapping test environment")
	// envtest.Environment: 띄울 임시 환경의 설정을 담은 구조체다.
	// &envtest.Environment{...} 는 구조체 값을 만든 뒤 그 주소(포인터)를 얻는 표현이다.
	//
	// 두 필드의 뜻은 다음과 같다.
	//   - CRDDirectoryPaths: 서버가 뜬 직후 여기 있는 CRD YAML들을 자동으로 설치한다.
	//     이 과정이 있어야 테스트가 NodeHealth 같은 커스텀 리소스를 만들 수 있다.
	//   - ErrorIfCRDPathMissing: 위 경로가 없으면 조용히 넘어가지 말고 즉시 에러를 내라는 뜻이다.
	//
	// filepath.Join("..", "..", "config", "crd", "bases")는 ../../config/crd/bases 경로를 조립한다.
	// ".."가 두 번인 이유는 go test의 작업 디렉터리가 테스트 파일이 있는 internal/controller이기 때문이다.
	// []string{...} 은 문자열 슬라이스 리터럴이며 경로를 여러 개 지정할 수도 있다.
	//
	// ErrorIfCRDPathMissing을 true로 두는 것이 중요하다.
	// false면 CRD가 하나도 설치되지 않은 채 서버가 떠서, 원인 파악이 어려운 "kind를 못 찾겠다" 실패로 이어진다.
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// IDE에서 테스트를 돌릴 수 있도록 처음 발견되는 binary directory를 가져옴.
	// Makefile을 거치면 KUBEBUILDER_ASSETS 환경 변수가 설정되지만 IDE의 실행 버튼은 그 과정을 건너뛴다.
	// 그래서 경로를 직접 찾아 채워 주는 것이며, 못 찾으면(빈 문자열이면) 아무것도 지정하지 않아 기본 탐색에 맡긴다.
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg는 이 파일에 전역으로 정의돼 있음.
	// testEnv.Start(): 여기서 실제로 etcd와 kube-apiserver 프로세스가 뜨고 CRD가 설치된다.
	// 수 초가 걸리는 무거운 호출이며, 접속 정보(rest.Config)와 에러를 함께 돌려준다.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	// 에러가 없더라도 cfg가 nil이 아닌지 한 번 더 확인하는 방어적 단언이다.
	// BeNil()은 "nil이다"를 뜻하므로 NotTo(BeNil())은 "nil이 아니어야 한다"가 된다.
	Expect(cfg).NotTo(BeNil())

	// client.New: 위 접속 정보로 실제 클라이언트를 만든다.
	// Scheme: scheme.Scheme 로 아까 CRD를 등록해 둔 그 전역 scheme을 넘기는 것이 핵심이다.
	// 이 클라이언트는 캐시 없이 API 서버를 직접 때리므로, 방금 쓴 값을 바로 읽는 테스트에 적합하다.
	// (매니저의 캐시 클라이언트는 반영이 늦어 테스트가 불안정해질 수 있다.)
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

// AfterSuite: 이 패키지의 마지막 스펙이 끝난 뒤 딱 한 번 실행되는 정리 훅이다.
//
// 설계 근거: envtest가 띄운 etcd와 kube-apiserver는 별도 OS 프로세스라 반드시 명시적으로 꺼야 한다.
// 이 훅을 빠뜨리면 테스트가 끝나도 프로세스와 임시 디렉터리가 남아 시스템 자원을 계속 잡아먹는다.
var _ = AfterSuite(func() {
	By("tearing down the test environment")
	// 먼저 스위트 ctx를 취소해 이 ctx를 쓰던 작업들에게 종료 신호를 보낸다.
	// 서버를 내리기 전에 소비자부터 멈추는 순서다.
	cancel()
	// Eventually: 조건이 만족될 때까지 주기적으로 다시 시도하는 gomega의 비동기 단언이다.
	// 인자는 (검사할 함수, 최대 대기 시간, 재시도 간격) 순이며, 여기서는 최대 1분 동안 1초 간격으로 재시도한다.
	// Should(Succeed())는 "넘긴 함수가 nil 에러를 돌려주면 성공"이라는 뜻이다.
	//
	// 단순히 testEnv.Stop()을 한 번만 부르지 않고 재시도하는 이유가 있다.
	// 프로세스 종료는 즉시 끝나지 않으며, 파일 잠금이나 포트 해제가 늦어져 첫 시도가 실패할 수 있다.
	// 이럴 때 재시도 없이 실패로 처리하면 정작 테스트는 다 통과했는데 정리 단계에서만 깨지는 불안정한 스위트가 된다.
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// getFirstFoundEnvTestBinaryDir: 지정 경로에서 처음 발견되는 binary를 찾는다.
// ENVTEST 기반 테스트는 특정 binary에 의존하고 보통 controller-runtime이 정한 경로에 위치하며,
// Makefile target 없이(예: IDE로) 테스트를 직접 돌릴 때는 'BinaryAssetsDirectory'를 명시적으로 지정해야 함.
//
// 이 함수는 'KUBEBUILDER_ASSETS' 환경 변수 설정과 비슷하게 필요한 binary를 찾아 과정을 간소화하며,
// binary가 제대로 준비되도록 사전에 'make setup-envtest' 실행 권장.
//
// Go 문법 설명:
//   - 인자가 없고 string 하나를 돌려주는 단순한 함수다.
//   - 소문자로 시작하므로 이 패키지 안에서만 보이는 비공개 함수다.
//   - 에러를 반환하지 않고 실패 시 빈 문자열("")을 돌려주는 설계다.
//     호출부가 "못 찾으면 그냥 기본값에 맡긴다"로 처리하면 되는, 실패해도 괜찮은 편의 기능이기 때문이다.
//
// 설계 근거: 여기서 찾는 binary는 etcd와 kube-apiserver 실행 파일이며, make setup-envtest가 bin/k8s 아래에 내려받아 둔다.
// 하위 디렉터리 이름에는 버전과 플랫폼이 붙어(예: 1.31.0-linux-amd64) 환경마다 달라진다.
// 이름을 코드에 박아 둘 수 없으므로 "무엇이 있든 처음 발견되는 하나"를 쓰는 방식으로 푼다.
func getFirstFoundEnvTestBinaryDir() string {
	// 탐색 시작 지점을 조립한다.
	// 작업 디렉터리가 internal/controller이므로 ../../bin/k8s 는 프로젝트 루트의 bin/k8s를 가리킨다.
	basePath := filepath.Join("..", "..", "bin", "k8s")
	// os.ReadDir: 그 디렉터리 안의 항목 목록과 에러를 함께 돌려준다.
	entries, err := os.ReadDir(basePath)
	if err != nil {
		// 디렉터리가 아예 없는 경우가 대표적이며, make setup-envtest를 아직 안 돌렸다는 뜻이다.
		// 여기서 테스트를 죽이지 않고 로그만 남긴 뒤 빈 문자열을 돌려준다.
		// KUBEBUILDER_ASSETS가 이미 설정된 CI 환경에서는 이 디렉터리가 없는 게 정상이기 때문이다.
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	// for _, entry := range entries : 목록을 훑되 인덱스는 필요 없으므로 _ 로 버리고 항목만 받는다.
	for _, entry := range entries {
		// 파일이 아니라 디렉터리인 첫 항목을 골라 그 전체 경로를 돌려준다.
		// return이 반복문 안에 있으므로 첫 디렉터리를 찾는 즉시 함수가 끝난다.
		if entry.IsDir() { // 첫 하위 directory를 binary 경로로 채택
			return filepath.Join(basePath, entry.Name())
		}
	}
	// 디렉터리는 읽혔지만 그 안에 하위 디렉터리가 하나도 없는 경우다.
	// 이때도 실패가 아니라 "못 찾음"이므로 빈 문자열을 돌려준다.
	return ""
}
