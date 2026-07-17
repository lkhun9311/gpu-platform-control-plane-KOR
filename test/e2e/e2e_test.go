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

// //go:build e2e 빌드 태그가 붙어 있으므로 이 파일은 go test -tags=e2e 로 태그를 켰을 때만 컴파일된다.
//
// 같은 태그가 e2e_suite_test.go에도 있어서 두 파일이 항상 함께 켜지고 함께 꺼진다.
//
// 태그가 없는 파일과 섞이면 한쪽만 빌드돼 컴파일 에러가 나므로 이 짝을 맞추는 것이 중요하다.
//
// package 선언: 이 파일은 e2e_suite_test.go와 같은 e2e 패키지에 속한다.
//
// 덕분에 저기 선언된 managerImage 전역 변수를 import 없이 그대로 쓸 수 있다.
package e2e

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
import (
	// encoding/json: JSON 문자열과 Go 구조체를 서로 변환하는 표준 패키지다.
	//
	// 아래에서 kubectl이 뱉은 TokenRequest 응답 JSON을 파싱하는 데 쓴다.
	"encoding/json"
	// fmt: 문자열 포매팅과 출력을 담당하며, Sprintf/Fprintf/Println을 여기서 쓴다.
	"fmt"
	// os: 파일을 쓰기 위해 사용한다(WriteFile, FileMode).
	"os"
	// os/exec: kubectl과 make를 자식 프로세스로 실행하는 표준 패키지다.
	"os/exec"
	// path/filepath: 운영체제에 맞는 방식으로 경로를 조합하는 표준 패키지다.
	"path/filepath"
	// time: 시간 값과 기간(Duration)을 다루며, 타임아웃 설정에 쓴다.
	"time"

	// ginkgo/gomega: 테스트 DSL과 단언 라이브러리이며, 점(.)을 붙인 dot import로 가져온다.
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// utils: 우리가 만든 e2e 테스트 헬퍼 패키지다(test/utils/utils.go).
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/test/utils"
)

// const 선언: 값이 바뀌지 않는 이름들을 상수로 고정한다.
//
// 이 이름들은 config/ 아래 kustomize 매니페스트가 실제로 만들어 내는 오브젝트 이름과 반드시 일치해야 한다.
//
// 매니페스트의 namePrefix를 바꾸면 여기도 같이 고쳐야 테스트가 대상을 찾을 수 있다.

// 프로젝트가 배포되는 namespace
//
// make deploy가 이 네임스페이스에 controller-manager를 올린다.
const namespace = "gpu-platform-control-plane-system"

// 프로젝트용으로 생성되는 service account
//
// controller Pod이 이 신원으로 API 서버와 대화하며, 아래 metrics 테스트에서 이 SA의 token을 발급받아 쓴다.
const serviceAccountName = "gpu-platform-control-plane-controller-manager"

// 프로젝트 metric service 이름
//
// controller가 노출하는 /metrics 엔드포인트 앞에 붙는 Service이며, 8443 포트로 HTTPS를 받는다.
const metricsServiceName = "gpu-platform-control-plane-controller-manager-metrics-service"

// metric data 조회 권한을 부여하려고 생성하는 RBAC 이름
//
// 테스트가 실행 중에 직접 만드는 ClusterRoleBinding의 이름이다.
const metricsRoleBindingName = "gpu-platform-control-plane-metrics-binding"

// Describe: 관련된 스펙들을 하나로 묶는 Ginkgo의 최상위 컨테이너다.
//
// Go 문법 설명.
//
//   - var _ = Describe(...) 형태인 이유는 Ginkgo가 "먼저 트리를 만들고 나중에 실행하는" 2단계 모델이기 때문이다.
//   - 패키지 전역 변수의 초기화식으로 두면 패키지 로드 시점에 Describe가 호출되어 스펙 트리에 등록된다.
//   - 등록만 하면 되고 반환값은 쓸 데가 없으므로 빈 식별자 _에 버린다.
//   - 안의 코드가 실제로 도는 시점은 TestE2E의 RunSpecs가 호출된 이후다.
//
// Ordered 데코레이터가 핵심이다.
//
// 기본적으로 Ginkgo는 스펙 순서를 무작위로 섞고 병렬로 돌릴 수도 있다.
//
// 하지만 여기 스펙들은 앞 단계가 만든 상태에 의존하므로(controllerPodName을 뒤 스펙이 쓴다) 순서가 반드시 보장돼야 한다.
//
// Ordered를 붙이면 선언된 순서대로 실행되고, 앞 스펙이 실패하면 뒤 스펙은 건너뛴다.
//
// 또한 Ordered 컨테이너 안에서만 BeforeAll/AfterAll을 쓸 수 있다.
//
// 문자열 "Manager"는 실행되는 코드 리터럴(리포트에 찍히는 이름)이므로 번역하지 않는다.
var _ = Describe("Manager", Ordered, func() {
	// controllerPodName: 첫 스펙이 찾아낸 controller Pod 이름을 담아 뒤의 스펙과 AfterEach가 함께 쓰는 변수다.
	//
	// Go 문법 설명.
	//
	//   - Describe에 넘긴 익명 함수 안에 선언했으므로 이 클로저의 지역 변수다.
	//   - 안쪽의 It/AfterEach 익명 함수들이 이 변수를 "닫아서 잡고(capture)" 있어서, 한 곳에서 대입한 값을 다른 곳에서 읽을 수 있다.
	//   - Pod 이름은 Deployment가 붙이는 무작위 접미사 때문에 미리 알 수 없어서, 실행 중에 조회해 여기 담아 둔다.
	var controllerPodName string

	// BeforeAll: 이 Ordered 컨테이너의 첫 스펙 전에 딱 한 번 실행되는 준비 블록이다.
	//
	// BeforeEach와 달리 스펙마다 반복되지 않으므로, 배포처럼 비싸고 한 번이면 충분한 작업에 알맞다.
	//
	// 테스트 실행 전 환경을 setup하고 namespace를 생성하며,
	// namespace에 restricted 보안 정책을 적용하고 CRD를 설치한 뒤,
	// controller를 배포한다.
	BeforeAll(func() {
		By("creating manager namespace")
		// kubectl create ns 로 배포 대상 네임스페이스를 만든다.
		//
		// 여기서 create가 실패하면(예: 이전 실행의 잔재로 이미 존재) 곧바로 알 수 있게 아래에서 단언한다.
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		// Pod Security Admission의 restricted 프로필을 이 네임스페이스에 강제한다.
		//
		// 이것이 e2e에서만 잡히는 대표적인 검증 항목이다.
		//
		// restricted는 root 실행 금지, 권한 상승 금지, 모든 capability 제거, seccomp 프로필 지정 등을 요구한다.
		//
		// manager 매니페스트의 securityContext가 이 요건을 하나라도 어기면 Pod 생성 자체가 거부된다.
		//
		// envtest에는 admission 웹훅도 스케줄러도 kubelet도 없어서 이런 위반을 절대 발견하지 못한다.
		//
		// --overwrite는 라벨이 이미 있어도 덮어쓰게 해서 명령을 멱등하게 만든다.
		//
		// cmd에 :=가 아니라 =를 쓰는 이유는 이미 위에서 선언된 변수라 재할당만 하면 되기 때문이다.
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		// make install은 config/crd 아래 매니페스트를 실제 API 서버에 등록한다.
		//
		// 이 단계는 "생성된 CRD YAML이 진짜 API 서버에 받아들여지는가"를 검증한다.
		//
		// OpenAPI 스키마가 잘못됐거나 CRD 이름이 너무 길거나 하는 문제는 여기서만 드러난다.
		//
		// envtest도 CRD를 로드하기는 하지만, 실제 클러스터의 검증과 버전 스큐까지 재현하지는 못한다.
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		// make deploy는 kustomize로 매니페스트를 빌드해 controller-manager를 클러스터에 올린다.
		//
		// IMG=<이미지>로 BeforeSuite에서 빌드하고 kind에 load한 그 이미지를 쓰게 한다.
		//
		// 이 한 줄이 배선 전체를 검증한다.
		//
		// RBAC이 부족하면 controller가 기동 직후 권한 에러로 죽고, Deployment 필드가 잘못되면 API 서버가 거부한다.
		//
		// envtest는 매니페스트를 아예 쓰지 않고 컨트롤러를 프로세스 안에서 직접 돌리므로 RBAC 누락을 구조적으로 못 잡는다.
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// AfterAll: 이 Ordered 컨테이너의 모든 스펙이 끝난 뒤 딱 한 번 실행되는 정리 블록이다.
	//
	// 중간에 스펙이 실패해도 실행되므로, 실패한 실행이 클러스터를 어지럽힌 채 끝나지 않게 해 준다.
	//
	// 모든 테스트 실행 후 정리하여 controller 배포를 해제하고 CRD를 제거한 뒤, namespace를 삭제한다.
	AfterAll(func() {
		// 여기 모든 명령이 _, _ = utils.Run(cmd) 형태로 에러를 버린다는 점에 주목한다.
		//
		// 정리 단계에서는 "이미 없어서 나는 에러"가 흔하고, 그 때문에 suite를 실패시키면 진짜 실패가 묻힌다.
		//
		// 게다가 앞 단계에서 실패했다면 뒤 리소스는 애초에 만들어지지도 않았을 수 있다.
		//
		// 그래서 최선을 다해 지우되, 실패하면 그냥 다음으로 넘어간다.
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		// BeforeAll의 make deploy를 되돌린다.
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		// BeforeAll의 make install을 되돌린다.
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		// 마지막으로 네임스페이스를 통째로 지운다.
		//
		// 네임스페이스 삭제는 그 안의 남은 리소스까지 함께 정리해 주는 안전망 역할을 한다.
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// AfterEach: 스펙 하나가 끝날 때마다 매번 실행되는 블록이다.
	//
	// 각 테스트 후 실패 여부를 확인하고, 디버깅용으로 log와 event, pod 상세 정보를 수집한다.
	//
	// 왜 필요한가: e2e 실패는 재현이 어렵고 특히 CI에서는 클러스터가 곧바로 사라진다.
	//
	// 그래서 실패한 그 순간에 진단 정보를 최대한 긁어모아 두지 않으면 원인 파악이 사실상 불가능해진다.
	//
	// 이 블록이 "사후 부검 자료"를 자동으로 남기는 역할을 한다.
	AfterEach(func() {
		// CurrentSpecReport()는 방금 끝난 스펙의 결과 정보를 담은 구조체를 돌려준다.
		specReport := CurrentSpecReport()
		// Failed()는 그 스펙이 실패했으면 true를 준다.
		//
		// 성공했을 때는 로그를 모을 필요가 없으므로 이 블록 전체를 건너뛴다.
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			// controller가 죽거나 잘못 동작한 이유는 대개 자기 로그에 적혀 있다.
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			// 진단 수집 자체가 실패해도 여기서 단언하지 않는 것이 중요하다.
			//
			// 이미 실패한 스펙 위에 또 실패를 얹으면 원래 실패 원인이 가려지기 때문이다.
			//
			// 그래서 성공하면 내용을 찍고, 실패하면 실패했다는 사실만 남긴다.
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			// Event는 로그에 안 나오는 것을 알려준다.
			//
			// 예를 들어 스케줄링 실패, ImagePullBackOff, Pod Security 위반으로 인한 거부는 Pod 로그가 아니라 Event에 남는다.
			//
			// --sort-by=.lastTimestamp로 시간순 정렬해 사건의 전개를 따라갈 수 있게 한다.
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			// curl -v 출력에는 TLS 핸드셰이크 과정과 HTTP 응답 헤더가 그대로 담긴다.
			//
			// metrics 스펙이 실패했다면 여기에 인증 거부(401/403)인지 연결 실패인지가 드러난다.
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			// kubectl describe는 Pod의 컨테이너 상태, 재시작 횟수, 종료 이유(OOMKilled 등), probe 실패를 한눈에 보여준다.
			//
			// Pod이 아예 뜨지 못한 경우에는 로그가 비어 있으므로 이 describe가 유일한 단서가 된다.
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				// 여기만 GinkgoWriter가 아니라 fmt.Println으로 표준출력에 바로 찍는다.
				//
				// GinkgoWriter는 조건에 따라 감춰질 수 있지만 Println은 항상 보이므로, 가장 중요한 진단 정보를 확실히 남기려는 의도다.
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	// Eventually의 기본 타임아웃과 폴링 주기를 이 컨테이너 범위에 설정한다.
	//
	// Go 문법 설명.
	//
	//   - 2 * time.Minute 은 시간 계산이 아니라 타입이 있는 상수 곱셈이다.
	//   - time.Minute은 time.Duration 타입 상수이며, 정수를 곱하면 그만큼의 기간이 된다.
	//   - 그래서 2 * time.Minute은 "2분"이라는 Duration 값이 된다.
	//
	// 왜 이런 값이 필요한가: 쿠버네티스는 본질적으로 비동기다.
	//
	// kubectl 명령이 성공했다는 건 "요청이 접수됐다"는 뜻이지 "그 결과가 실현됐다"는 뜻이 아니다.
	//
	// 이미지 pull, 스케줄링, 컨테이너 기동에는 시간이 걸리므로 즉시 단언하면 반드시 실패한다.
	//
	// 그래서 조건이 참이 될 때까지 1초 간격으로 최대 2분간 재시도하는 것이 e2e의 기본 문법이다.
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	// Context: Describe와 기능은 완전히 같고, 읽는 사람에게 "어떤 상황인지"를 알려주는 의미상의 구분일 뿐이다.
	//
	// 문자열은 실행되는 코드 리터럴이므로 번역하지 않는다.
	Context("Manager", func() {
		// It: 실제 하나의 테스트 스펙(검증 단위)이다.
		//
		// Ginkgo 리포트에서 "Manager Manager should run successfully" 처럼 바깥 이름들과 이어 붙어 한 문장으로 읽힌다.
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			// verifyControllerUp: 검증 로직을 함수 값으로 만들어 변수에 담는다.
			//
			// Go 문법 설명.
			//
			//   - func(g Gomega) { ... } 는 이름 없는 함수(클로저)이며, Go에서 함수는 값이라 변수에 넣을 수 있다.
			//   - 인자로 받는 g Gomega가 이 패턴의 핵심이다.
			//   - 전역 Expect를 쓰면 단언이 실패하는 순간 스펙 전체가 즉시 실패해 버린다.
			//   - 반면 g.Expect를 쓰면 실패가 "이번 시도의 실패"로만 처리되어, Eventually가 조용히 다시 시도할 수 있다.
			//   - 즉 g를 받는 함수는 Eventually에 넘겨 재시도시키기 위한 형태다.
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				// -l control-plane=controller-manager 라벨 셀렉터로 우리 controller Pod만 고른다.
				//
				// -o go-template=... 는 출력 형식을 직접 지정하는 것이다.
				//
				// 이 템플릿은 "삭제 중이 아닌(deletionTimestamp가 없는) Pod의 이름만 한 줄씩" 출력한다.
				//
				// deletionTimestamp를 거르는 이유가 중요하다.
				//
				// 재배포 직후에는 종료 중인 예전 Pod과 새 Pod이 잠시 함께 보이는데, 그걸 거르지 않으면 아래 HaveLen(1)이 간헐적으로 실패한다.
				//
				// 문자열을 + 로 이어붙인 것은 한 줄이 너무 길어져서 나눈 것일 뿐이며, 실제로는 하나의 인자다.
				//
				// \\n 은 Go 문자열 안에서 역슬래시와 n을 뜻하고, 그게 go-template 문법에서 개행으로 해석된다.
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				// 출력 끝의 개행 때문에 생기는 빈 줄을 걸러야 아래 개수 검증이 정확해진다.
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running") // controller pod는 정확히 1개
				// 찾아낸 이름을 바깥 클로저 변수에 담아, 뒤의 스펙과 AfterEach가 쓸 수 있게 한다.
				//
				// podNames[0]은 슬라이스의 첫 요소이며, 바로 위에서 길이가 1임을 확인했으므로 안전하다.
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				// -o jsonpath={.status.phase}는 Pod 상태에서 phase 값 하나만 뽑아 출력한다.
				//
				// go-template보다 간단해서 단일 필드를 뽑을 때 즐겨 쓴다.
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Running이 아니면 이번 시도는 실패로 처리되고, Eventually가 1초 뒤 다시 부른다.
				//
				// 이 단언이 잡아내는 것: 이미지가 실제로 컨테이너에서 실행 가능한가, 매니페스트의 securityContext가 restricted 정책을 통과하는가.
				//
				// CrashLoopBackOff나 ImagePullBackOff면 phase가 영영 Running이 되지 않아 2분 뒤 타임아웃으로 실패한다.
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			// Eventually(함수)는 그 함수가 성공할 때까지 주기적으로 반복 호출한다.
			//
			// Should(Succeed())는 "g.Expect 단언이 하나도 실패하지 않고 함수가 끝나야 한다"는 뜻이다.
			//
			// 타임아웃과 폴링 주기는 위에서 설정한 기본값(2분, 1초)을 따른다.
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			// metrics 엔드포인트는 인증/인가로 보호된다(아무나 못 읽는다).
			//
			// 그래서 controller의 ServiceAccount에게 metrics-reader ClusterRole을 묶어 준다.
			//
			// --serviceaccount=<네임스페이스>:<이름> 형식이라 Sprintf로 조립한다.
			//
			// 이 단계가 검증하는 것: metrics 보호 설정이 실제로 작동한다는 사실 그 자체다.
			//
			// 권한을 주지 않으면 아래 curl이 403을 받는데, 그건 곧 "보호가 켜져 있다"는 증거이기도 하다.
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=gpu-platform-control-plane-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			// Service가 매니페스트대로 실제로 만들어졌는지 확인한다.
			//
			// 여기가 실패하면 kustomize 설정에서 metrics service가 빠졌다는 뜻이다.
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			// 아래 정의된 헬퍼로 SA의 Bearer token을 발급받는다.
			//
			// curl이 이 token으로 자신을 증명해야 metrics를 읽을 수 있다.
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			// BeEmpty()는 "비어 있다"를 검사하는 matcher이며, NotTo로 뒤집어 "비어 있으면 안 된다"가 된다.
			//
			// 발급이 조용히 실패해 빈 문자열이 오면, 뒤에서 원인을 알 수 없는 401로 나타나므로 여기서 미리 막는다.
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			// phase가 Running인 것과 Ready 조건이 True인 것은 다르다.
			//
			// Running은 "컨테이너가 시작됐다"일 뿐이고, Ready는 "readiness probe를 통과해 트래픽을 받을 수 있다"는 뜻이다.
			//
			// metrics를 긁으려면 Ready여야 하므로 여기서 한 단계 더 기다린다.
			//
			// 이것도 e2e에서만 검증되는 항목이다.
			//
			// readiness probe의 경로나 포트가 잘못 설정됐다면 오직 실제 kubelet만이 그 사실을 알려줄 수 있다.
			verifyControllerPodReady := func(g Gomega) {
				// jsonpath의 [?(@.type=='Ready')] 는 필터 표현식이며, conditions 배열에서 type이 Ready인 항목만 골라 그 status를 뽑는다.
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			// Eventually(함수, 타임아웃, 폴링주기) 형태로 기본값 대신 개별 값을 넘길 수 있다.
			//
			// 기본 2분 대신 3분을 주는 이유는 첫 실행에서 이미지 pull과 기동에 시간이 더 걸릴 수 있기 때문이다.
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			// Pod이 Ready라고 해서 metrics 서버가 반드시 떠 있는 건 아니다.
			//
			// 그래서 controller 로그에 "Serving metrics server" 문구가 찍혔는지를 직접 확인한다.
			//
			// 로그 문자열에 의존하는 다소 취약한 방식이지만, 커넥션을 열어 보기 전에 "서버가 진짜 떴다"를 값싸게 확인할 수 있다.
			//
			// 이 대기를 빼면 curl이 아직 리스닝하지 않는 포트에 붙으려다 연결 거부로 간헐 실패한다.
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// ContainSubstring은 "이 문자열이 안에 들어 있다"를 검사하는 matcher다.
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness
			//
			// 이 주석은 kubebuilder 스캐폴딩 마커다.
			//
			// 사람이 읽는 설명이 아니라, kubebuilder가 코드를 생성할 때 "여기에 코드를 끼워 넣어라"라고 표시해 둔 지점이다.
			//
			// 지우면 이후 코드 생성이 제대로 동작하지 않으므로 그대로 두어야 한다.

			By("creating the curl-metrics pod to access the metrics endpoint")
			// metrics를 클러스터 밖에서 긁는 대신, 클러스터 안에 curl Pod을 띄워 안에서 긁는다.
			//
			// 왜 안에서 긁는가: metrics Service는 cluster-internal DNS 이름으로만 접근 가능하고, TLS도 클러스터 내부 인증서를 쓴다.
			//
			// 밖에서 port-forward로 접근하면 실제 서비스 경로가 아니라 우회로를 검증하는 셈이 된다.
			//
			// 안에서 긁어야 "다른 Pod이 실제로 metrics를 읽을 수 있는가"라는 진짜 질문에 답할 수 있다.
			//
			// --restart=Never는 이 Pod을 한 번 돌고 끝나는 작업(Job 비슷하게)으로 만든다.
			//
			// 그래야 아래에서 phase가 Succeeded가 되기를 기다릴 수 있다.
			//
			// 기본값인 Always면 curl이 끝나자마자 재시작해서 영영 Succeeded가 되지 않는다.
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				// --overrides로 Pod 스펙 일부를 JSON으로 직접 덮어쓴다.
				//
				// securityContext처럼 kubectl run의 플래그만으로는 표현할 수 없는 필드를 넣어야 하기 때문이다.
				"--overrides",
				//
				// Go 문법 설명(백틱 문자열).
				//
				//   - 역따옴표(`)로 감싼 것은 raw string literal이다.
				//   - 안의 큰따옴표를 역슬래시로 이스케이프하지 않아도 되고, 줄바꿈도 그대로 문자열에 들어간다.
				//   - JSON처럼 큰따옴표가 잔뜩 들어가는 내용에 아주 잘 맞는다.
				//   - 다만 백틱 안에는 백틱 자신을 넣을 수 없다.
				//
				// 이 JSON의 %s 네 자리는 아래 인자로 순서대로 채워진다: token, metricsServiceName, namespace, serviceAccountName.
				//
				// args의 셸 명령은 재시도 루프다.
				//
				// 2초 간격으로 최대 30번 curl을 시도하고, 성공하면 exit 0, 끝내 실패하면 exit 1로 끝난다.
				//
				// 이 Pod 안의 재시도가 필요한 이유는, 바깥 Eventually가 Pod을 다시 만들어 주지는 않기 때문이다.
				//
				// 즉 Pod이 뜬 시점에 metrics 서버가 아직 준비 전이더라도 Pod 스스로 기다려 준다.
				//
				// curl -k 는 인증서 검증을 건너뛴다.
				//
				// metrics 서버는 자체 서명 인증서를 쓰는데 curl 이미지에 그 CA가 없기 때문이며, 여기서 검증 대상은 TLS 신뢰 체인이 아니라 인증/인가와 metrics 노출이다.
				//
				// -H 'Authorization: Bearer <token>' 이 바로 위에서 발급받은 SA token으로 자신을 증명하는 부분이다.
				//
				// URL의 <서비스>.<네임스페이스>.svc.cluster.local 은 쿠버네티스 내부 DNS 이름이며, 이 이름이 풀리는지 자체도 검증 대상이다.
				//
				// securityContext는 위에서 네임스페이스에 강제한 restricted 정책을 만족시키려고 명시한 것이다.
				//
				// readOnlyRootFilesystem, allowPrivilegeEscalation=false, 모든 capability drop, runAsNonRoot, seccompProfile 중 하나라도 빠지면 이 Pod은 생성 자체가 거부된다.
				//
				// serviceAccountName을 controller의 SA로 지정해야 위에서 만든 ClusterRoleBinding의 권한을 물려받는다.
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			// Succeeded는 "컨테이너가 종료 코드 0으로 끝났다"는 뜻이다.
			//
			// 위 셸 루프가 curl 성공 시 exit 0을 하므로, Succeeded는 곧 "metrics를 최소 한 번 성공적으로 받아왔다"는 신호다.
			//
			// curl이 30번 모두 실패하면 exit 1이 되어 phase는 Failed가 되고, 이 단언은 끝내 통과하지 못한다.
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			// 타임아웃만 5분으로 늘리고 폴링 주기는 기본값(1초)을 쓴다.
			//
			// Eventually는 이렇게 인자를 일부만 넘기는 것도 허용한다.
			//
			// 5분이 필요한 이유는 curl 이미지 pull에 더해 안쪽 재시도 루프가 최대 60초 정도 돌 수 있기 때문이다.
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			// Pod이 Succeeded인 것만으로도 사실상 확인은 됐지만, 여기서 실제 응답 내용까지 눈으로 확인한다.
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				// "< HTTP/1.1 200 OK" 에서 앞의 < 는 curl -v 가 "서버로부터 받은 줄"임을 표시하는 기호다.
				//
				// 즉 이 문자열이 있다는 건 서버가 실제로 200을 응답했다는 뜻이다.
				//
				// 이 한 줄이 전체 사슬(DNS 해석, TLS 연결, token 인증, RBAC 인가, metrics 서빙)이 끝까지 이어졌음을 증명한다.
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK")) // curl 응답에 200 OK 포함 확인
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
		//
		// kubebuilder 스캐폴딩 마커이며, webhook 관련 검증 코드가 생성될 때 삽입되는 지점이다.
		//
		// 사람이 지우면 안 되는 표식이다.

		// TODO: 프로젝트에 특화된 시나리오로 e2e 테스트 suite를 customize할 때는 sample/CR을 적용해 상태를 확인하거나 metric으로 reconcile 동작을 검증하는 방식을 고려한다.
		// 예:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
		//
		// 즉 GPUQuotaPolicy 같은 실제 CR을 만든 뒤, controller_runtime_reconcile_total metric이 올라가는지 확인하면
		// "reconcile이 실제 클러스터에서 정말 돌았다"를 metric으로 증명할 수 있다는 제안이다.
	})
})

// serviceAccountToken: 지정한 namespace의 해당 service account token을 반환한다.
//
// Kubernetes TokenRequest API로 직접 요청을 보내 token을 생성하고, API 응답에서 token을 파싱한다.
//
// 왜 필요한가: 예전 쿠버네티스는 ServiceAccount마다 token이 담긴 Secret을 자동으로 만들어 줬지만, 지금은 그렇지 않다.
//
// 요즘은 TokenRequest API로 짧은 수명의 token을 그때그때 발급받는 방식이 표준이다.
//
// 이 헬퍼가 그 발급 절차를 감싸서, 테스트는 token 문자열 하나만 받아 쓰면 되게 해 준다.
func serviceAccountToken() (string, error) {
	// TokenRequest API에 보낼 요청 본문이다.
	//
	// spec을 비워 두면 만료 시간 등은 서버 기본값이 적용된다.
	//
	// 역따옴표(`)로 감싼 raw string literal이라 안의 큰따옴표를 이스케이프하지 않아도 되고 줄바꿈도 그대로 들어간다.
	//
	// 함수 안에서도 const를 선언할 수 있으며, 그 경우 이 함수 안에서만 보인다.
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	// kubectl create --raw 는 요청 본문을 파일에서 읽으므로, 위 JSON을 임시 파일에 먼저 써야 한다.
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	// filepath.Join은 운영체제에 맞는 구분자로 경로를 이어 붙인다.
	//
	// 문자열을 직접 "+"로 붙이는 것보다 안전한 관례적 방식이다.
	tokenRequestFile := filepath.Join("/tmp", secretName)
	// os.WriteFile(경로, 내용, 권한)로 파일을 통째로 쓴다.
	//
	// []byte(문자열)은 문자열을 바이트 슬라이스로 변환하는 표현이며, WriteFile이 []byte를 받기 때문에 필요하다.
	//
	// 0o644는 8진수 리터럴이며 "소유자는 읽기/쓰기, 나머지는 읽기" 권한을 뜻한다(0o 접두사가 8진수 표시다).
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		// 첫 반환값은 token 문자열 자리인데 줄 값이 없으므로 빈 문자열을 넣는다.
		//
		// Go에서 string의 제로값이 ""이므로 이것이 관례다.
		return "", err
	}

	// out: 클로저 안에서 얻은 token을 바깥으로 빼내기 위한 변수다.
	//
	// Eventually에 넘기는 함수는 값을 반환할 수 없으므로(Gomega가 정한 형태), 이렇게 바깥 변수에 대입하는 방식을 쓴다.
	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		// kubectl create --raw <API 경로> -f <본문 파일> 은 kubectl을 통해 임의의 API를 직접 호출하는 방법이다.
		//
		// TokenRequest는 전용 kubectl 서브커맨드가 마땅치 않아서 이 방식을 쓴다.
		//
		// Sprintf로 네임스페이스와 SA 이름을 경로에 채워 넣는다.
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		// 여기서는 utils.Run이 아니라 cmd.CombinedOutput()을 직접 부른다는 점에 주목한다.
		//
		// utils.Run은 출력에 "running: ..." 같은 로그를 섞지는 않지만 에러에 출력을 덧붙인다.
		//
		// 여기서는 출력이 반드시 순수한 JSON이어야 json.Unmarshal이 성공하므로, 가공 없는 원본 출력이 필요하다.
		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		// var token tokenRequest 로 아래에 정의된 구조체의 빈 값을 만든다.
		var token tokenRequest
		//
		// Go 문법 설명(JSON 역직렬화).
		//
		//   - json.Unmarshal(데이터, 대상포인터)는 JSON 바이트를 Go 구조체에 채워 넣는다.
		//   - &token 의 & 는 주소(포인터)를 뜻하며, Unmarshal이 token 원본을 채워야 하므로 포인터가 필수다.
		//   - 값으로 넘기면 복사본만 채워지고 원본은 그대로라, Unmarshal은 아예 에러를 낸다.
		//   - 구조체에 없는 JSON 필드는 조용히 무시되므로, 필요한 필드만 정의해도 된다.
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		// 파싱한 token 문자열을 바깥 변수에 담는다.
		out = token.Status.Token
	}
	// Eventually로 감싸는 이유는 SA가 방금 만들어졌다면 token 발급이 잠깐 실패할 수 있기 때문이다.
	//
	// 기본 타임아웃(2분)과 폴링 주기(1초)를 따른다.
	Eventually(verifyTokenCreation).Should(Succeed())

	// out과 err를 함께 돌려준다.
	//
	// 여기서 err는 위 os.WriteFile의 err이며 이 지점에서는 항상 nil이다(nil이 아니면 이미 return했다).
	//
	// 클로저 안의 err는 별개의 변수라 여기까지 전달되지 않는다.
	return out, err
}

// getMetricsOutput: metric endpoint 접근에 쓰인 curl pod의 log를 조회해 반환
//
// 왜 필요한가: curl Pod의 로그를 읽는 동작이 스펙 안과 위 TODO 예시 양쪽에서 쓰인다.
//
// 같은 명령을 여러 번 적는 대신 함수 하나로 뽑아 두면, 나중에 Pod 이름이나 네임스페이스가 바뀌어도 한 곳만 고치면 된다.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	// curl Pod은 이미 종료(Succeeded)됐지만, 로그는 Pod 오브젝트가 지워지기 전까지 남아 있어 그대로 읽을 수 있다.
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	// utils.Run의 반환값 (string, error)가 이 함수의 반환 타입과 정확히 같으므로 그대로 돌려주면 된다.
	return utils.Run(cmd)
}

// tokenRequest: Kubernetes TokenRequest API 응답을 간략히 표현한 구조체로, 추출에 필요한 token 필드만 포함한다.
//
// Go 문법 설명(구조체와 JSON 태그).
//
//   - type 이름 struct { ... } 는 여러 필드를 묶는 사용자 정의 타입 선언이다.
//   - 소문자로 시작하는 tokenRequest는 이 패키지 안에서만 보이는 비공개 타입이다.
//   - 필드 뒤 역따옴표 안의 `json:"token"` 은 "구조체 태그"이며, 실행 코드가 아니라 리플렉션으로 읽히는 메타데이터다.
//   - 이 태그가 "JSON의 token 키를 이 필드에 매핑하라"고 encoding/json에게 알려준다.
//   - 필드 이름은 반드시 대문자로 시작해야 한다(Token, Status).
//     소문자면 패키지 밖인 encoding/json이 접근할 수 없어서 파싱이 조용히 실패한다.
//     이것이 Go 초보가 자주 겪는 함정이다.
//   - 안쪽에 이름 없는 struct를 그대로 중첩했다(익명 구조체).
//     한 번만 쓰는 형태라 굳이 별도 타입 이름을 만들지 않은 것이다.
//
// 실제 API 응답에는 metadata, spec, status.expirationTimestamp 등이 더 들어 있지만, 필요한 status.token 하나만 정의했다.
//
// json.Unmarshal은 구조체에 없는 필드를 조용히 무시하므로 이렇게 최소한만 정의해도 잘 동작한다.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
