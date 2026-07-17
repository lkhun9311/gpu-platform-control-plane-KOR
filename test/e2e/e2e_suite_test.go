//go:build e2e
// +build e2e

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

// 파일 맨 위의 //go:build e2e 는 "빌드 태그(build tag)"라고 부르는 코드다(주석처럼 생겼지만 컴파일러가 읽는다).
//
// Go 문법 설명(빌드 태그).
//
//   - //go:build <조건> 은 "이 조건이 참일 때만 이 파일을 컴파일 대상에 넣어라"는 지시다.
//   - 여기 조건이 e2e이므로 이 파일은 go test -tags=e2e 처럼 태그를 켰을 때만 빌드된다.
//   - 그냥 go build ./... 나 go test ./... 를 하면 이 파일은 아예 없는 것처럼 무시된다.
//   - 아래 // +build e2e 는 Go 1.17 이전 도구를 위한 옛 형식이며, 둘을 함께 두는 것이 관례다.
//   - 두 줄 사이와 아래에 빈 줄이 반드시 있어야 하고, 없으면 태그가 아니라 그냥 주석으로 취급된다.
//
// 왜 태그를 거는가: e2e 테스트는 실제 kind cluster, docker, kubectl이 있어야만 돌아간다.
//
// 태그가 없으면 CI의 일반 유닛 테스트 단계에서 같이 실행되려 하다가 환경이 없어 실패한다.
//
// 태그로 격리해 두면 "필요할 때만 명시적으로 켜서 돌리는 테스트"가 된다.
//
// 이 때문에 go vet ./test/... 도 기본적으로는 이 파일을 검사하지 않는다.
package e2e

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
import (
	// fmt: 문자열 포매팅과 출력을 담당하며, Sprintf/Fprintf를 여기서 쓴다.
	"fmt"
	// os: 환경변수를 읽고 쓰기 위해 사용한다(Getenv/Setenv).
	"os"
	// os/exec: make 같은 외부 명령을 자식 프로세스로 실행하는 표준 패키지다.
	"os/exec"
	// testing: Go 표준 테스트 프레임워크이며, *testing.T 타입이 여기서 온다.
	//
	// Ginkgo도 결국은 표준 go test 위에 얹혀 돌아가므로 이 패키지가 진입점 역할을 한다.
	"testing"

	// ginkgo: BDD 스타일 테스트 프레임워크이며, RunSpecs/BeforeSuite/By 등이 여기서 온다.
	//
	// 점(.)을 붙인 dot import라서 ginkgo.RunSpecs가 아니라 RunSpecs로 바로 쓸 수 있다.
	. "github.com/onsi/ginkgo/v2"
	// gomega: Ginkgo와 짝을 이루는 단언(assertion) 라이브러리이며, Expect/HaveOccurred 등이 여기서 온다.
	//
	// 역시 dot import를 써서 Expect(err).NotTo(HaveOccurred()) 가 영어 문장처럼 읽히게 만든다.
	. "github.com/onsi/gomega"

	// utils: 우리가 만든 e2e 테스트 헬퍼 패키지다(test/utils/utils.go).
	//
	// Run, LoadImageToKindClusterWithName, InstallCertManager 등이 여기 있다.
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/test/utils"
)

// var 블록: 패키지 전역 변수들을 한 번에 선언한다.
//
// Go 문법 설명.
//
//   - 소문자로 시작하므로 이 변수들은 e2e 패키지 안에서만 보이는 비공개 값이다.
//   - 여기 있는 managerImage는 같은 패키지의 e2e_test.go에서도 import 없이 그대로 쓸 수 있다.
var (
	// 테스트용으로 빌드하고 load할 manager image
	//
	// example.com 이라는 실재하지 않는 레지스트리를 일부러 쓴다.
	//
	// 이렇게 하면 어떤 이유로든 kind가 이미지를 못 찾았을 때 조용히 외부에서 진짜 이미지를 당겨오는 사고가 원천 차단된다.
	//
	// 즉 "kind load가 제대로 됐는지"를 테스트가 확실히 검증하게 된다.
	managerImage = "example.com/gpu-platform-control-plane:v0.0.1"
	// 이 suite가 CertManager를 설치했는지 여부 추적
	//
	// 이 플래그가 필요한 이유는 정리 단계에서 "내가 설치한 것만 지운다"를 지키기 위해서다.
	//
	// 개발자의 클러스터에 원래 있던 cert-manager를 테스트가 지워버리면 안 된다.
	shouldCleanupCertManager = false
)

// TestE2E: go test가 찾아서 실행하는 표준 테스트 함수이며, Ginkgo suite 전체의 진입점이다.
//
// 격리된 환경에서 솔루션을 검증하는 e2e 테스트 suite를 실행하고, 기본 setup은 Kind와 CertManager를 필요로 한다.
//
// kubectl kuberc(custom kubectl 설정 사용)를 활성화하려면 KUBECTL_KUBERC=true를 설정한다.
//
// kuberc는 기본적으로 비활성이며, 이는 환경별로 일관된 테스트 동작을 보장하기 위함이다.
//
// CertManager 설치를 건너뛰려면 CERT_MANAGER_INSTALL_SKIP=true를 설정한다.
//
// Go 문법 설명(표준 테스트 함수).
//
//   - go test는 이름이 Test로 시작하고 *testing.T 하나를 인자로 받는 함수를 자동으로 찾아 실행한다.
//   - 파일 이름이 _test.go로 끝나야 테스트 파일로 인식된다.
//   - 즉 Ginkgo suite도 결국 이 함수 하나를 통해 표준 go test에 올라타는 구조다.
//
// Go 문법 설명(Ginkgo suite 구조).
//
//   - Ginkgo는 "먼저 트리를 만들고, 그 다음에 실행한다"는 2단계 모델로 동작한다.
//   - 1단계(트리 구성): 패키지의 var _ = Describe(...) 들이 초기화되면서 스펙 트리가 메모리에 만들어진다.
//   - 2단계(실행): 아래 RunSpecs가 호출되면 그때 비로소 BeforeSuite -> 각 스펙 -> AfterSuite 순으로 실제로 돌아간다.
func TestE2E(t *testing.T) {
	// RegisterFailHandler는 Gomega에게 "단언이 실패하면 이 함수를 불러라"라고 알려준다.
	//
	// 여기 넘기는 Fail은 Ginkgo의 실패 처리 함수이며, 이 연결이 Gomega와 Ginkgo를 이어 붙이는 접착제다.
	//
	// Go 문법 설명: Go에서 함수는 값이라서, Fail을 괄호 없이 이름만 쓰면 "함수 자체"를 인자로 넘기는 것이 된다.
	//
	// Fail()처럼 괄호를 붙이면 그 자리에서 호출해 버리므로 완전히 다른 의미가 된다.
	RegisterFailHandler(Fail)
	// suite 시작을 알리는 배너를 남긴다.
	//
	// GinkgoWriter는 스펙이 실패했을 때만 내용을 보여주므로 성공한 실행의 출력을 어지럽히지 않는다.
	//
	// 반환값 (쓴 바이트 수, 에러)는 로그 출력이라 신경 쓸 필요가 없으므로 _, _ = 로 둘 다 버린다.
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gpu-platform-control-plane e2e test suite\n")
	// RunSpecs가 실제 실행 방아쇠다.
	//
	// t를 넘겨야 Ginkgo가 스펙 실패를 표준 go test의 실패로 보고할 수 있다.
	//
	// 두 번째 인자 "e2e suite"는 리포트에 찍히는 suite 이름이며, 이건 실행되는 코드 리터럴이므로 번역하지 않는다.
	RunSpecs(t, "e2e suite")
}

// BeforeSuite: suite 전체에서 딱 한 번, 첫 스펙이 돌기 전에 실행되는 준비 블록이다.
//
// 여기서 이미지 빌드, kind load, cert-manager 설치처럼 비싸고 한 번이면 충분한 일을 처리한다.
//
// Go 문법 설명.
//
//   - var _ = BeforeSuite(func() { ... }) 라는 다소 낯선 모양의 이유가 있다.
//   - BeforeSuite는 함수라서 어딘가에서 반드시 호출돼야 하는데, Ginkgo에는 등록 전용 main 함수가 없다.
//   - 그래서 패키지 전역 변수의 초기화식으로 두어, 패키지가 로드될 때 자동으로 호출되게 만든다.
//   - 반환값은 쓸 데가 없으므로 빈 식별자 _에 버린다(변수 이름을 붙이면 "안 쓰는 변수"가 되어 거슬린다).
//   - func() { ... } 는 이름 없는 함수(익명 함수)이며, 이걸 값처럼 인자로 넘긴다.
var _ = BeforeSuite(func() {
	// By는 지금 어느 단계를 지나고 있는지 리포트에 남기는 Ginkgo 함수다.
	//
	// 실패했을 때 "어디까지 갔다가 터졌는지"를 바로 알 수 있어서 e2e처럼 단계가 긴 테스트에서 특히 유용하다.
	//
	// 안의 문자열은 실행되는 코드 리터럴이므로 번역하지 않는다.
	By("building the manager image")
	// exec.Command로 make docker-build IMG=<이미지> 를 서술한다.
	//
	// fmt.Sprintf로 "IMG=example.com/..." 형태의 인자 하나를 만들어 넘긴다.
	//
	// 여기서는 아직 실행되지 않고, 아래 utils.Run 안에서 실제로 실행된다.
	//
	// 이 단계가 중요한 이유는 e2e가 소스가 아니라 "실제 컨테이너 이미지"를 검증 대상으로 삼기 때문이다.
	//
	// Dockerfile이 깨졌거나 바이너리가 정적 링크되지 않아 컨테이너 안에서 못 도는 문제는 envtest로는 절대 잡히지 않는다.
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	// utils.Run은 프로젝트 루트에서 명령을 실행하고, 표준출력+표준에러를 합쳐 돌려준다.
	//
	// 출력 문자열은 여기서 쓸 데가 없으므로 _로 버리고 에러만 받는다.
	_, err := utils.Run(cmd)
	// ExpectWithOffset(1, ...)은 Expect와 같지만, 실패 위치를 한 단계 위(호출자)로 보고한다.
	//
	// Go 문법 설명(Gomega 단언 읽는 법).
	//
	//   - Expect(값).NotTo(Matcher) 는 "값이 이 조건에 맞지 않아야 한다"는 뜻이다.
	//   - HaveOccurred()는 "에러가 발생했다"를 검사하는 matcher다.
	//   - 그래서 Expect(err).NotTo(HaveOccurred())는 "에러가 나지 않아야 한다"로 읽는다.
	//   - 마지막 문자열은 실패했을 때 함께 출력할 설명이다.
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	// TODO(user): e2e 테스트 vendor를 Kind에서 바꾸고 싶다면,
	// image가 빌드되어 사용 가능한지 확인한 뒤 아래 block을 제거한다.
	//
	// 즉 kind가 아니라 진짜 레지스트리를 쓰는 환경이라면, 이미지를 push하는 방식이 되므로 이 load 단계가 필요 없어진다.
	By("loading the manager image on Kind")
	// kind 노드는 호스트의 도커 이미지 저장소를 공유하지 않으므로, 방금 빌드한 이미지를 노드 안으로 직접 복사해 넣어야 한다.
	//
	// 이 단계를 빼면 controller Pod이 ImagePullBackOff 상태로 영영 뜨지 않는다.
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	// kubectl의 사용자 설정이 테스트에 끼어들지 못하게 막는다.
	configureKubectlKubeRC()
	// webhook 인증서 발급에 필요한 cert-manager를 준비한다.
	setupCertManager()
})

// AfterSuite: suite의 모든 스펙이 끝난 뒤 딱 한 번 실행되는 정리 블록이다.
//
// 중간에 스펙이 실패해도 실행되므로, 실패한 실행이 클러스터를 더럽힌 채 끝나는 일을 막아 준다.
var _ = AfterSuite(func() {
	// 이 suite가 직접 설치한 경우에만 cert-manager를 제거한다(판단은 함수 안에서 한다).
	teardownCertManager()
})

// configureKubectlKubeRC: kubectl kuberc 기능을 기본적으로 꺼서 테스트를 개발자 개인 설정으로부터 격리한다.
//
// 테스트 격리를 위해 기본적으로 kubectl kuberc를 비활성화한다.
//
// local kubectl 설정이 테스트 동작에 영향을 주는 것을 방지한다.
//
// kuberc를 활성화하려면 KUBECTL_KUBERC=true를 설정한다.
//
// 왜 필요한가: kuberc는 kubectl의 기본 플래그나 별칭을 사용자가 미리 정해 두는 기능이다.
//
// 개발자 로컬에 kuberc 설정이 있으면 테스트가 실행하는 kubectl 명령의 동작이 슬쩍 달라질 수 있다.
//
// 그러면 "내 머신에서는 되는데 CI에서는 안 되는" 재현 불가능한 실패가 생긴다.
//
// 그래서 명시적으로 꺼서 어디서 돌리든 같은 동작을 보장한다.
func configureKubectlKubeRC() {
	// os.Getenv는 환경변수가 없으면 빈 문자열을 돌려준다.
	//
	// 즉 "명시적으로 true라고 켠 경우가 아니면" 이라는 조건이 된다.
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		// os.Setenv는 이 테스트 프로세스의 환경변수를 바꾼다.
		//
		// utils.Run이 os.Environ()을 자식 프로세스에 물려주므로, 여기서 세팅한 값이 kubectl에까지 전달된다.
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		// 무엇을 왜 했는지 리포트에 남긴다.
		//
		// 문자열이 길어서 Fprintf 인자를 다음 줄로 내렸을 뿐이며, 동작은 한 줄일 때와 같다.
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		// 사용자가 일부러 켠 경우이므로 존중하고 그대로 둔다.
		//
		// 다만 나중에 이상한 동작이 보일 때 원인을 찾을 수 있도록 기록은 남긴다.
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// setupCertManager: webhook 테스트에 필요하면 CertManager를 설치하고, CERT_MANAGER_INSTALL_SKIP=true이거나 이미 설치돼 있으면 설치를 건너뛴다.
//
// 왜 필요한가: webhook은 TLS 인증서가 있어야 API 서버의 호출을 받을 수 있고, 이 프로젝트는 그 발급을 cert-manager에 맡긴다.
//
// envtest는 인증서를 내부적으로 알아서 처리해 주지만, 실제 클러스터에서는 진짜 발급자가 있어야 한다.
//
// "인증서 배선이 실제로 맞는가"는 오직 e2e에서만 검증되는 항목이다.
func setupCertManager() {
	// 첫 번째 탈출구: 사용자가 명시적으로 건너뛰라고 지시한 경우다.
	//
	// CI에서 cert-manager를 미리 깔아두는 파이프라인이라면 이 단계가 시간 낭비이므로 끄고 싶을 수 있다.
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		// return 문 하나로 함수를 즉시 끝낸다(반환값이 없는 함수이므로 뒤에 값을 적지 않는다).
		return
	}

	By("checking if CertManager is already installed")
	// 두 번째 탈출구: 이미 클러스터에 cert-manager가 있는 경우다.
	//
	// 이때 shouldCleanupCertManager를 false로 남겨두는 것이 핵심이다.
	//
	// 우리가 설치하지 않았으니 정리도 하지 않아야 하며, 그래야 남의 cert-manager를 지우는 사고가 나지 않는다.
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// 중단이나 부분 설치에 대비해 설치 전에 정리 대상으로 표시
	//
	// 순서가 중요하다.
	//
	// 설치가 성공한 뒤에 표시하면, 설치 도중에 실패하거나 중단됐을 때 이미 만들어진 리소스가 정리 없이 남는다.
	//
	// 미리 표시해 두면 부분 설치 상태여도 AfterSuite가 지우려고 시도한다.
	shouldCleanupCertManager = true

	By("installing CertManager")
	// Expect(값).To(Succeed())는 "error를 돌려주는 함수 호출이 성공(nil)해야 한다"를 검사하는 관용구다.
	//
	// Succeed()는 error 타입 전용 matcher이며, InstallCertManager()가 error 하나만 돌려주므로 이 형태가 잘 맞는다.
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager: 설치한 경우에만 CertManager를 제거하여, 우리가 설치한 것만 지우도록 보장한다.
//
// 왜 필요한가: e2e 테스트는 개발자의 실제 클러스터를 상대할 수 있으므로 "남의 것을 건드리지 않는다"는 원칙이 중요하다.
//
// 이 함수가 setupCertManager에서 세운 플래그를 그대로 존중하는 짝이 된다.
func teardownCertManager() {
	// ! 는 논리 부정 연산자이며, "설치한 적이 없다면" 이라는 뜻이 된다.
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	// UninstallCertManager는 반환값이 없고 내부에서 실패를 경고로만 남긴다.
	//
	// 정리 단계에서 나는 "이미 없음" 류의 에러로 suite 전체를 실패시키면 진짜 실패가 묻히기 때문이다.
	utils.UninstallCertManager()
}
