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
//
// 같은 디렉터리(test/utils)의 모든 .go 파일은 반드시 같은 package 이름(utils)을 가져야 한다.
//
// 이 패키지는 e2e 테스트가 공통으로 쓰는 헬퍼 모음이며, test/e2e 쪽에서 import해 사용한다.
//
// 이 파일에는 //go:build e2e 태그가 없다는 점이 중요하다.
//
// 태그가 없으므로 이 패키지는 항상 컴파일 대상이 되고, 그래서 go vet ./test/... 로 문법 검증이 가능하다.
package utils

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
//
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// bufio: 버퍼를 끼운 입출력 유틸이며, 여기서는 Scanner로 문자열을 한 줄씩 읽는 데 쓴다.
	"bufio"
	// bytes: 바이트 슬라이스를 다루는 표준 패키지이고, 여기서는 메모리 버퍼(bytes.Buffer)를 만든다.
	"bytes"
	// fmt: 문자열 포매팅과 출력을 담당하며, Sprintf/Fprintf/Errorf를 여기서 쓴다.
	"fmt"
	// os: 운영체제와 대화하는 표준 패키지이고, 환경변수/작업디렉터리/파일 읽고쓰기에 쓴다.
	"os"
	// os/exec: 외부 프로그램(kubectl, kind, make 등)을 자식 프로세스로 실행하는 표준 패키지다.
	//
	// e2e 테스트는 실제 클러스터를 상대하므로 Go 코드가 아니라 CLI를 호출하는 방식이 자연스럽다.
	"os/exec"
	// strings: 문자열 검색/치환/분리 유틸이며, Join/Contains/Index/TrimPrefix/SplitSeq를 쓴다.
	"strings"

	// ginkgo: BDD 스타일 테스트 프레임워크이며, 여기서는 GinkgoWriter만 사용한다.
	//
	// Go 문법 설명.
	//
	//   - import 경로 앞의 점(.)은 "dot import"라고 부른다.
	//   - 보통은 ginkgo.GinkgoWriter처럼 패키지 이름을 붙여야 하지만, 점을 붙이면 GinkgoWriter처럼 바로 쓸 수 있다.
	//   - Ginkgo는 Describe/It/By 같은 DSL을 문장처럼 읽히게 하려고 공식적으로 dot import를 권장한다.
	//   - 다만 dot import는 일반적으로는 안티패턴이라 린터가 경고하므로, 뒤의 // nolint:revive,staticcheck 주석으로 그 경고를 끈다.
	//   - nolint 주석은 실행되는 코드가 아니라 golangci-lint에게 주는 지시문이다.
	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

// const 블록: 값이 절대 바뀌지 않는 상수들을 한 번에 선언한다.
//
// Go 문법 설명.
//
//   - const는 컴파일 시점에 값이 확정되는 상수이며, 실행 중 재할당이 불가능하다.
//   - 소문자로 시작하므로 이 상수들은 utils 패키지 안에서만 보이는 비공개 값이다.
//   - 이런 값을 상수로 빼두면 버전을 올릴 때 이 한 곳만 고치면 된다는 장점이 있다.
const (
	// certmanagerVersion: 설치할 cert-manager 릴리스 버전이다.
	//
	// 버전을 고정(pin)하는 이유는 latest를 쓰면 업스트림 릴리스가 나올 때마다 테스트가 예고 없이 깨질 수 있기 때문이다.
	certmanagerVersion = "v1.20.2"
	// certmanagerURLTmpl: cert-manager 매니페스트 다운로드 URL의 템플릿 문자열이다.
	//
	// 안의 %s 자리에 위 certmanagerVersion이 fmt.Sprintf로 채워진다.
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	// defaultKindBinary: kind 실행 파일 이름의 기본값이며, PATH에서 찾는다.
	//
	// 환경변수 KIND로 덮어쓸 수 있어서 프로젝트가 bin/kind에 내려받은 바이너리를 쓰게 만들 수도 있다.
	defaultKindBinary = "kind"
	// defaultKindCluster: 대상 kind cluster 이름의 기본값이다.
	//
	// 환경변수 KIND_CLUSTER로 덮어쓸 수 있다.
	defaultKindCluster = "kind"
)

// warnError: 에러를 실패로 처리하지 않고 경고 문구로만 남기는 헬퍼다.
//
// Go 문법 설명.
//
//   - 반환 타입이 없는 함수이므로 return 문 없이 끝난다.
//   - fmt.Fprintf(w, format, args...)는 지정한 곳(w)에 포맷 문자열을 써 넣는다.
//   - GinkgoWriter는 Ginkgo가 제공하는 출력 대상이며, 스펙이 실패했을 때만 내용을 보여주는 특성이 있다.
//     그래서 성공한 테스트의 출력으로 로그를 어지럽히지 않는다.
//   - _, _ = 로 반환값 두 개를 모두 버린다.
//     Go는 쓰지 않는 변수를 컴파일 에러로 막지만, 빈 식별자 _에 넣으면 "의도적으로 버린다"는 표시가 된다.
//     Fprintf는 (쓴 바이트 수, 에러)를 돌려주는데 로그 출력 실패는 테스트에서 신경 쓸 일이 아니므로 버린다.
//
// 왜 필요한가: 정리(cleanup) 단계에서는 "이미 지워져 있어서 나는 에러"가 흔하다.
//
// 그런 에러로 테스트 전체를 실패시키면 진짜 실패가 묻히므로, 경고만 남기고 계속 진행하는 통로가 필요하다.
func warnError(err error) {
	// %v는 값을 기본 형식으로 출력하는 포맷 지정자이며, 에러에 쓰면 Error() 문자열이 나온다.
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run: 주어진 명령을 프로젝트 루트에서 실행하고, 표준출력과 표준에러를 합친 결과를 문자열로 돌려준다.
//
// 이 context 안에서 주어진 명령 실행
//
// Go 문법 설명.
//
//   - 대문자로 시작하는 Run은 공개(export) 함수이므로 다른 패키지(test/e2e)에서 utils.Run(...)으로 부를 수 있다.
//   - 인자 *exec.Cmd는 "실행할 명령을 서술한 구조체"의 포인터다.
//     호출하는 쪽이 exec.Command("kubectl", "get", "pods")처럼 만들어서 넘긴다.
//     포인터로 받기 때문에 이 함수 안에서 cmd.Dir이나 cmd.Env를 바꾸면 원본에 그대로 반영된다.
//   - 반환 타입 (string, error)는 "출력 문자열"과 "에러"를 함께 돌려준다는 뜻이다.
//
// 왜 필요한가: e2e 테스트 전반이 exec.Command로 CLI를 부른다.
//
// 매번 작업 디렉터리 맞추기, 환경변수 세팅, 출력 캡처, 에러에 명령 문자열 붙이기를 반복하면 중복이 심하다.
//
// 이 함수 하나로 그 공통 절차를 모으고, 실패했을 때 "무슨 명령이 어떤 출력을 내며 실패했는지"를 항상 알 수 있게 만든다.
func Run(cmd *exec.Cmd) (string, error) {
	// 프로젝트 루트 경로를 구한다.
	// 두 번째 반환값(error)은 _로 버리는데, 실패해도 dir에 현재 작업 디렉터리가 담겨 오므로 그대로 진행할 수 있다.
	dir, _ := GetProjectDir()
	// cmd.Dir에 값을 넣으면 자식 프로세스가 그 디렉터리에서 시작한다.
	// make install 같은 명령은 Makefile이 있는 루트에서 실행돼야 하므로 이 설정이 필수다.
	cmd.Dir = dir

	// os.Chdir는 이 테스트 프로세스 자신의 작업 디렉터리도 옮긴다.
	// cmd.Dir만으로 자식 프로세스는 해결되지만, 상대 경로를 쓰는 코드(config/ 아래 파일 참조 등)를 위해 부모도 맞춰준다.
	// if err := ...; err != nil 은 호출과 동시에 err를 만들고 즉시 검사하는 Go의 관용구이며, err는 이 if 블록 안에서만 유효하다.
	if err := os.Chdir(cmd.Dir); err != nil {
		// 여기서 return하지 않는 이유는, 디렉터리 이동이 실패해도 cmd.Dir 덕분에 명령 자체는 성공할 수 있기 때문이다.
		// 그래서 실패를 기록만 하고 계속 진행한다.
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	// cmd.Env는 자식 프로세스에게 넘길 환경변수 목록("KEY=VALUE" 문자열의 슬라이스)이다.
	// os.Environ()으로 현재 프로세스의 환경 전체를 가져온 뒤 GO111MODULE=on을 덧붙인다.
	// append(슬라이스, 추가값...)은 슬라이스 끝에 요소를 붙여 새 슬라이스를 돌려주는 내장 함수다.
	// 주의: cmd.Env에 값을 넣는 순간 상속이 끊기고 이 목록만 쓰이므로, os.Environ()을 기반으로 삼는 것이 중요하다.
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	// cmd.Args는 명령과 인자가 담긴 문자열 슬라이스다(예: ["kubectl","get","pods"]).
	// strings.Join으로 공백을 끼워 하나의 문자열로 합쳐 사람이 읽을 수 있는 명령줄을 만든다.
	command := strings.Join(cmd.Args, " ")
	// %q는 값을 큰따옴표로 감싸 출력하는 포맷 지정자이며, 공백이 섞인 명령의 경계를 눈으로 확인하기 좋다.
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	// CombinedOutput()은 명령을 실제로 실행하고 끝날 때까지 기다린 뒤, 표준출력과 표준에러를 하나로 합친 바이트를 돌려준다.
	//
	// Go 문법 설명(os/exec 출력 캡처).
	//
	//   - Output()은 표준출력만 캡처한다.
	//   - CombinedOutput()은 표준출력 + 표준에러를 합쳐서 캡처한다.
	//   - kubectl과 make는 진짜 에러 메시지를 표준에러로 내보내므로, 진단이 목적이라면 CombinedOutput이 맞다.
	//   - 반환값은 []byte(바이트 슬라이스)라서 사람이 읽으려면 string(...)으로 변환해야 한다.
	output, err := cmd.CombinedOutput()
	if err != nil {
		// 명령이 0이 아닌 종료 코드로 끝난 경우다.
		//
		// Go 문법 설명(에러 래핑).
		//
		//   - fmt.Errorf는 포맷 문자열로 새 에러를 만든다.
		//   - 그중 %w 지정자는 "원본 에러를 감싼다(wrap)"는 특수 지정자다.
		//   - 감싸두면 나중에 errors.Is/errors.As로 원본 에러(예: *exec.ExitError)를 꺼내 검사할 수 있다.
		//   - %v로 넣으면 문자열로 납작해져서 원본 타입 정보가 사라지므로 %w가 더 낫다.
		//
		// 실패했을 때도 output을 함께 돌려주는 것이 핵심이다.
		//
		// 호출자가 "종료 코드 1" 같은 무의미한 정보 대신 kubectl이 실제로 뭐라고 했는지 볼 수 있다.
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	// 정상 경로: 출력 문자열과 "에러 없음"을 뜻하는 nil을 함께 돌려준다.
	return string(output), nil
}

// UninstallCertManager: 클러스터에서 cert-manager를 제거한다.
//
// cert manager 제거
//
// Go 문법 설명.
//
//   - 반환값이 없다는 점에 주목한다.
//   - 정리 함수라서 개별 실패를 호출자에게 올리지 않고 내부에서 warnError로 삼켜버리기 때문이다.
//
// 왜 필요한가: e2e suite가 직접 설치한 cert-manager는 suite가 끝날 때 직접 치워야 한다.
//
// 남겨두면 다음 실행이나 같은 클러스터를 쓰는 다른 테스트의 전제를 오염시킨다.
func UninstallCertManager() {
	// 상수 템플릿의 %s에 버전을 채워 실제 매니페스트 URL을 만든다.
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	// exec.Command(name, arg...)는 명령을 "서술"만 할 뿐 아직 실행하지는 않는다.
	// 실제 실행은 위 Run 안의 CombinedOutput() 시점에 일어난다.
	// kubectl delete -f <url>은 설치할 때 apply했던 리소스들을 그대로 되돌려 지운다.
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		// 이미 지워져 있거나 일부만 남아 있어도 여기서 에러가 난다.
		// 정리 단계이므로 실패시키지 않고 경고만 남긴다.
		warnError(err)
	}

	// kube-system에 남은 lease 삭제, 기본적으로는 정리되지 않음
	//
	// cert-manager는 리더 선출(leader election)에 Lease 오브젝트를 쓰는데, 이 Lease는 cert-manager 네임스페이스가 아니라 kube-system에 만들어진다.
	//
	// 그래서 위의 kubectl delete -f 로는 지워지지 않고 남는다.
	//
	// 남은 Lease가 있으면 나중에 cert-manager를 재설치했을 때 새 리더가 만료를 기다리느라 기동이 느려질 수 있다.
	//
	// Go 문법 설명.
	//
	//   - []string{...}는 문자열 슬라이스 리터럴이며, 값들을 나열해 바로 초기화한다.
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	// for _, lease := range 슬라이스 는 슬라이스의 각 요소를 순회하는 반복문이다.
	// range는 (인덱스, 값)을 주는데 인덱스가 필요 없으므로 첫 자리를 _로 버린다.
	for _, lease := range kubeSystemLeases {
		// --ignore-not-found: 대상이 없어도 에러를 내지 않게 한다(멱등성 확보).
		// --force --grace-period=0: 유예 시간 없이 즉시 삭제해 정리가 지연되지 않게 한다.
		// 여기서 :=가 아니라 =를 쓰는 이유는 cmd가 이미 위에서 선언된 변수이므로 재할당만 하면 되기 때문이다.
		cmd = exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager: cert-manager 번들을 설치하고 webhook이 준비될 때까지 기다린다.
//
// cert manager bundle 설치
//
// Go 문법 설명.
//
//   - 반환 타입이 error 하나뿐이다.
//   - 설치는 반드시 성공해야 하는 사전 준비 단계라, 실패를 삼키지 않고 호출자(BeforeSuite)에게 올려 suite를 즉시 실패시킨다.
//
// 왜 필요한가: webhook에는 TLS 인증서가 필요하고, 이 프로젝트는 그 인증서 발급을 cert-manager에 맡긴다.
//
// envtest는 인증서를 자체적으로 조작해 주므로 cert-manager가 필요 없지만, 실제 클러스터에서는 진짜 발급자가 있어야 한다.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	// kubectl apply -f <url>로 원격 매니페스트를 그대로 클러스터에 적용한다.
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		// 설치 자체가 실패했으면 뒤의 대기는 의미가 없으므로 즉시 에러를 올린다.
		return err
	}
	// cert-manager-webhook가 준비될 때까지 대기하며, cluster에서 제거 후 재설치한 경우 시간이 걸릴 수 있다.
	//
	// apply가 성공했다는 건 "오브젝트가 API 서버에 등록됐다"는 뜻일 뿐 "Pod이 실제로 뜨고 요청을 받을 준비가 됐다"는 뜻이 아니다.
	//
	// 이 대기를 빼면 뒤이은 Certificate 생성이 "webhook에 연결할 수 없다"는 에러로 간헐적으로 실패한다.
	//
	// 이런 종류의 타이밍 버그가 바로 envtest로는 절대 잡히지 않고 e2e에서만 드러나는 문제다.
	//
	// kubectl wait --for condition=Available은 지정한 조건이 True가 될 때까지 블로킹한다.
	//
	// --timeout 5m은 5분 안에 안 되면 포기하고 실패하라는 뜻이며, 무한정 매달리는 것을 막는 안전장치다.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	// 출력 문자열은 여기서 쓸 데가 없으므로 _로 버리고 에러만 받는다.
	_, err := Run(cmd)
	// err를 그대로 돌려준다(nil이면 성공, 아니면 실패).
	return err
}

// IsCertManagerCRDsInstalled: Cert Manager 관련 핵심 CRD의 존재 여부를 확인해, Cert Manager CRD가 설치돼 있는지 검사한다.
//
// Go 문법 설명.
//
//   - 반환 타입 bool은 참/거짓 값이며, 에러를 돌려주지 않는다.
//   - 조회에 실패한 경우도 그냥 false로 취급한다("모르면 설치 안 된 것으로 본다").
//
// 왜 필요한가: 개발자의 로컬 클러스터에는 이미 cert-manager가 깔려 있을 수 있다.
//
// 그 위에 다시 설치하면 남의 설정을 덮어쓰고, suite가 끝날 때 남의 것까지 지워버리게 된다.
//
// 그래서 설치 전에 먼저 물어보고, 이미 있으면 손대지 않는다.
func IsCertManagerCRDsInstalled() bool {
	// 흔한 Cert Manager CRD 목록
	//
	// 이 중 하나만 보여도 cert-manager가 있다고 판단한다.
	//
	// 여러 개를 나열하는 이유는 버전에 따라 CRD 구성이 조금씩 다를 수 있기 때문이다.
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// kubectl 명령을 실행해 전체 CRD 조회
	//
	// 개별 CRD를 하나씩 kubectl get 하면 왕복이 6번 필요하다.
	//
	// 한 번에 전부 받아 문자열에서 찾는 편이 훨씬 빠르다.
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		// 클러스터에 접근조차 못 하는 상황이다.
		// 여기서 true를 돌려주면 "설치돼 있다"고 착각해 설치 단계를 건너뛰게 되므로, 안전한 쪽인 false를 고른다.
		return false
	}

	// Cert Manager CRD 중 하나라도 존재하는지 확인
	//
	// kubectl get 출력은 헤더 한 줄과 리소스마다 한 줄로 되어 있어서, 줄 단위로 쪼개면 다루기 쉽다.
	crdList := GetNonEmptyLines(output)
	// 중첩 반복문으로 "찾는 CRD 이름" x "출력 줄"의 모든 조합을 확인한다.
	// 목록이 6개 x 수십 줄 수준이라 이 정도 완전 탐색으로 충분하다.
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			// strings.Contains(전체, 부분)은 부분 문자열이 들어있으면 true를 준다.
			// 정확히 일치(==)로 비교하지 않는 이유는 각 줄이 "이름 CREATED AT" 형태라 이름 뒤에 다른 열이 붙어 있기 때문이다.
			if strings.Contains(line, crd) {
				// 하나만 찾아도 결론이 나므로 남은 반복을 다 돌지 않고 즉시 반환한다.
				return true
			}
		}
	}

	// 끝까지 하나도 못 찾았으면 설치돼 있지 않다는 뜻이다.
	return false
}

// LoadImageToKindClusterWithName: 로컬에서 빌드한 docker image를 kind cluster 안으로 밀어 넣는다.
//
// local docker image를 kind cluster로 load
//
// 왜 필요한가: kind cluster의 노드는 컨테이너라서, 호스트의 도커 이미지 저장소를 공유하지 않는다.
//
// 그래서 방금 docker build로 만든 이미지를 kind 노드는 볼 수 없고, Pod은 ImagePullBackOff로 멈춘다.
//
// 레지스트리에 push하는 대신 kind load docker-image로 이미지를 노드 안으로 직접 복사하면 이 문제를 우회할 수 있다.
//
// 그래서 매니페스트의 imagePullPolicy가 Never 또는 IfNotPresent여야 이 방식이 성립한다.
func LoadImageToKindClusterWithName(name string) error {
	// 기본 cluster 이름에서 시작한다.
	cluster := defaultKindCluster
	//
	// Go 문법 설명.
	//
	//   - os.LookupEnv는 (값, 존재여부) 두 개를 돌려준다.
	//   - os.Getenv는 없을 때 빈 문자열을 주므로 "값이 없음"과 "빈 값으로 설정됨"을 구분하지 못한다.
	//   - LookupEnv의 ok로 그 둘을 구분할 수 있어서 여기서는 LookupEnv를 쓴다.
	//   - if v, ok := ...; ok { } 는 변수를 만들면서 바로 검사하는 관용구다.
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		// 환경변수가 있으면 그 값으로 덮어쓴다.
		// CI에서 여러 cluster를 병렬로 굴릴 때 유용하다.
		cluster = v
	}
	// kind에 넘길 인자 목록을 슬라이스로 미리 만든다.
	// kind load docker-image <이미지> --name <cluster> 형태의 명령이 된다.
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	// kind 실행 파일도 같은 방식으로 환경변수 덮어쓰기를 지원한다.
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	//
	// Go 문법 설명(가변인자 펼치기).
	//
	//   - exec.Command의 시그니처는 Command(name string, arg ...string)이며, ...string이 가변인자다.
	//   - 가변인자에는 인자를 몇 개든 나열해 넘길 수 있다.
	//   - 이미 슬라이스를 갖고 있다면 슬라이스 뒤에 ...을 붙여 "요소들을 하나씩 펼쳐서 넘긴다".
	//   - kindOptions만 넘기면 "슬라이스 한 개"를 넘기는 게 되어 타입 에러가 나므로 ...이 반드시 필요하다.
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines: 주어진 명령 출력 문자열을 줄바꿈 기준으로 개별 항목으로 나누고, 그중 빈 요소는 무시한다.
//
// Go 문법 설명.
//
//   - 반환 타입 []string은 "문자열 슬라이스"이며, 길이가 유동적인 배열이라고 생각하면 된다.
//
// 왜 필요한가: CLI 출력은 거의 항상 마지막에 개행이 붙는다.
//
// 그냥 "\n"으로 쪼개면 맨 끝에 빈 문자열이 하나 생겨서 개수 검증(HaveLen(1))이 예상 밖으로 실패한다.
//
// 이 헬퍼로 빈 줄을 걸러 두면 호출자는 개수와 인덱스를 안심하고 쓸 수 있다.
func GetNonEmptyLines(output string) []string {
	// var res []string 는 nil 슬라이스를 선언한다.
	//
	// nil 슬라이스에도 append를 쓸 수 있어서 make로 미리 만들 필요가 없다.
	var res []string
	// strings.SplitSeq는 문자열을 구분자로 쪼개되, 슬라이스를 한꺼번에 만들지 않고 하나씩 흘려보내는 이터레이터를 돌려준다.
	//
	// 큰 출력에서 중간 슬라이스 할당을 아낄 수 있는 비교적 최근에 추가된 API다.
	elements := strings.SplitSeq(output, "\n")
	// for element := range 이터레이터 형태다.
	//
	// 슬라이스를 range할 때와 달리 여기서는 인덱스가 아니라 값이 바로 나온다는 점이 다르다(range-over-func).
	for element := range elements {
		// 빈 문자열이 아닌 줄만 결과에 담는다.
		if element != "" {
			// append는 슬라이스 끝에 요소를 붙인 새 슬라이스를 돌려주므로, 반드시 결과를 다시 대입해야 한다.
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir: 프로젝트 루트 디렉터리 경로를 돌려준다.
//
// 프로젝트가 위치한 directory 반환
//
// 왜 필요한가: go test는 테스트 파일이 있는 디렉터리(test/e2e)를 작업 디렉터리로 삼아 실행한다.
//
// 하지만 make install이나 make deploy는 Makefile이 있는 루트에서 돌아야 한다.
//
// 그래서 Run이 명령을 실행하기 전에 이 함수로 루트를 찾아 작업 디렉터리를 옮긴다.
func GetProjectDir() (string, error) {
	// os.Getwd는 현재 작업 디렉터리의 절대 경로를 돌려준다.
	wd, err := os.Getwd()
	if err != nil {
		// 실패해도 wd를 그대로 함께 돌려준다.
		//
		// 호출자인 Run이 에러를 무시하고 wd를 쓰더라도 최소한 빈 문자열보다는 낫기 때문이다.
		//
		// %w로 원본 에러를 감싸 원인 추적이 가능하게 한다.
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	// 경로에서 "/test/e2e" 부분을 통째로 지워 루트를 얻는다.
	// strings.ReplaceAll(전체, 옛것, 새것)은 일치하는 모든 부분을 바꾼다.
	//
	// 문자열을 잘라내는 이 방식은 다소 거친 휴리스틱이다.
	//
	// 이미 루트에서 호출되면 "/test/e2e"가 없으므로 아무것도 안 바뀌고 그대로 루트가 나온다(그래서 동작한다).
	//
	// kubebuilder가 만들어 주는 관례적인 구현이며, e2e 테스트가 test/e2e에 있다는 전제에 의존한다.
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode: 파일에서 target을 찾아 해당 내용의 주석 접두사를 제거하며, target 내용은 여러 줄에 걸칠 수 있다.
//
// Go 문법 설명.
//
//   - (filename, target, prefix string)처럼 같은 타입의 인자는 타입을 마지막에 한 번만 적어도 된다.
//   - filename: 수정할 파일 경로다.
//   - target: 파일 안에서 찾을, 주석 처리된 코드 덩어리 전체다.
//   - prefix: 각 줄 앞에서 떼어낼 주석 표시다(예: "// " 또는 "#").
//
// 왜 필요한가: kubebuilder 스캐폴딩은 webhook 설정 같은 선택적 블록을 주석 처리한 상태로 만들어 둔다.
//
// 특정 시나리오를 e2e로 검증하려면 테스트가 실행 중에 그 블록을 되살려야 한다.
//
// 이 헬퍼는 그 "코드 주석 해제"를 프로그램으로 수행한다(주로 config/ 아래 kustomize 파일이 대상이다).
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	//
	// gosec 린터는 변수로 받은 경로를 파일로 읽는 것을 파일 경로 조작 취약점으로 의심한다.
	//
	// 여기서는 테스트가 자기 리포지토리 안의 파일을 읽을 뿐이므로 오탐(false positive)이며, nolint로 경고를 끈다.
	//
	// os.ReadFile은 파일 전체를 []byte로 한 번에 읽어 온다.
	content, err := os.ReadFile(filename)
	if err != nil {
		// %q로 파일명을 따옴표에 넣고, %w로 원본 에러를 감싸 어떤 파일에서 왜 실패했는지 남긴다.
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	// 문자열 검색을 하려고 []byte를 string으로 변환한다.
	//
	// 이 변환은 내용을 복사하므로 원본 content는 그대로 남아 있고, 아래에서 바이트 슬라이싱에 계속 쓴다.
	strContent := string(content)

	// strings.Index는 target이 처음 나타나는 위치(바이트 오프셋)를 돌려주며, 없으면 -1을 준다.
	idx := strings.Index(strContent, target)
	if idx < 0 {
		// 찾는 블록이 없다는 건 스캐폴딩이 바뀌었거나 인자가 틀렸다는 뜻이므로 조용히 넘어가지 않고 에러를 낸다.
		return fmt.Errorf("unable to find the code %q to be uncommented", target)
	}

	// out: 새 파일 내용을 조립할 메모리 버퍼다.
	//
	// new(T)는 T의 제로값을 만들고 그 포인터를 돌려주는 내장 함수이며, bytes.Buffer는 제로값 그대로 바로 쓸 수 있다.
	//
	// 문자열을 += 로 계속 이어붙이면 매번 새 문자열이 생겨 비효율적이라, 버퍼에 쌓는 방식을 쓴다.
	out := new(bytes.Buffer)
	// content[:idx]는 슬라이싱 표현이며, "처음부터 idx 직전까지"를 뜻한다.
	//
	// 즉 target 앞부분은 손대지 않고 그대로 옮겨 적는다.
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// bufio.Scanner는 입력을 한 줄씩 읽어 주는 도구다.
	//
	// bytes.NewBufferString(target)으로 target 문자열을 읽을 수 있는 입력으로 감싼 뒤 Scanner에 물린다.
	//
	// target이 여러 줄일 수 있으므로 줄 단위로 처리해야 각 줄에서 prefix를 뗄 수 있다.
	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	// Scan()은 다음 줄을 읽고, 읽을 게 있으면 true를 준다.
	//
	// 첫 Scan()이 false면 target이 비어 있다는 뜻이므로 아무것도 하지 않고 성공으로 끝낸다.
	if !scanner.Scan() {
		return nil
	}
	// for { ... } 는 조건이 없는 무한 반복문이며, 안에서 break로 빠져나온다.
	//
	// 여기서 무한 루프를 쓰는 이유는 "줄을 쓰고 -> 다음 줄이 있는지 보고 -> 있으면 개행을 넣는" 순서가 필요하기 때문이다.
	//
	// for scanner.Scan() 형태로 쓰면 마지막 줄 뒤에 개행이 하나 더 붙는 문제를 피하기 어렵다.
	for {
		// scanner.Text()는 방금 읽은 줄을 개행 없이 문자열로 준다.
		//
		// strings.TrimPrefix(문자열, 접두사)는 접두사가 있으면 떼고, 없으면 원본을 그대로 돌려준다(안전하다).
		//
		// 이 한 줄이 바로 "주석 해제"의 실체다.
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// target의 마지막 줄이었던 경우 개행을 쓰지 않도록 함
		//
		// 먼저 다음 줄이 있는지 확인하고, 없으면 개행을 넣지 않은 채 빠져나온다.
		//
		// 이렇게 해야 원본 target이 차지하던 범위와 정확히 같은 모양이 유지된다.
		if !scanner.Scan() {
			break
		}
		// 다음 줄이 있다는 게 확인됐으므로 줄 사이 구분자로 개행을 넣는다.
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	// content[idx+len(target):]는 "target이 끝나는 지점부터 파일 끝까지"를 뜻한다.
	//
	// target 뒷부분도 손대지 않고 그대로 옮겨 적는다.
	//
	// len(target)은 문자 수가 아니라 바이트 길이이며, idx도 바이트 오프셋이므로 단위가 맞아 올바르게 동작한다.
	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	//
	// 이번에는 gosec이 파일 권한 0644를 지적하지만, 테스트가 만드는 설정 파일이라 문제가 없다.
	//
	// os.WriteFile은 파일을 통째로 덮어쓰며, out.Bytes()로 버퍼에 쌓아 둔 최종 내용을 꺼내 넘긴다.
	//
	// 0644는 8진수 리터럴이며 "소유자는 읽기/쓰기, 나머지는 읽기"라는 유닉스 권한을 뜻한다.
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	// 여기까지 왔으면 전부 성공했으므로 "에러 없음"을 뜻하는 nil을 돌려준다.
	return nil
}
