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

// package main: 이 패키지는 라이브러리가 아니라 "실행 가능한 프로그램"이라는 뜻이다.
// Go는 package 이름이 정확히 main이고 그 안에 func main()이 있는 패키지만 실행 파일로 빌드한다.
// 이 파일은 controller-manager 바이너리의 진입점이다.
//
// 설계 근거(설계서 Components 절): 이 프로젝트는 바이너리를 두 개로 나눈다.
//   - cmd/main.go: controller-manager (컨트롤 플레인). CRD를 watch하고 reconcile해서 원하는 상태를 맞춘다.
//   - cmd/gateway/main.go: 서빙 게이트웨이 (데이터 플레인). 사용자 추론 트래픽을 실시간으로 받아 넘긴다.
//
// 두 바이너리를 나누는 이유는 부하 특성과 장애 영향 범위가 완전히 다르기 때문이다.
// 게이트웨이는 요청량에 따라 수평 확장해야 하지만 컨트롤러는 leader election으로 한 replica만 활성이어야 한다.
// 또 게이트웨이가 트래픽 폭주로 죽어도 컨트롤러의 reconcile은 계속 돌아야 하고, 그 반대도 마찬가지다.
// 한 프로세스에 합치면 이 두 요구를 동시에 만족시킬 수 없다.
package main

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
// 관례상 (1)표준 라이브러리, (2)서드파티, (3)이 프로젝트 내부 순으로 빈 줄로 그룹을 나눈다.
import (
	// crypto/tls: TLS(HTTPS) 설정 타입을 담은 표준 패키지다.
	// 아래에서 HTTP/2를 끄기 위해 tls.Config를 손본다.
	"crypto/tls"
	// flag: 커맨드라인 인자(--metrics-bind-address 등)를 파싱하는 표준 패키지다.
	"flag"
	// os: 운영체제와 상호작용하는 표준 패키지이며, 여기서는 os.Exit(1)로 프로세스를 끝낼 때 쓴다.
	"os"

	// 밑줄(_)로 시작하는 import는 "블랭크 임포트(blank import)"라고 부른다.
	// 패키지의 심볼을 하나도 쓰지 않지만 그 패키지의 init() 함수만은 실행되게 하고 싶을 때 쓴다.
	// (Go는 쓰지 않는 import를 컴파일 에러로 막는데, _ 를 붙이면 그 검사를 통과한다.)
	// 여기서는 인증 plugin들이 자기 자신을 client-go에 등록하는 부수효과만 필요하다.
	//
	// 모든 Kubernetes client 인증 plugin (Azure, GCP, OIDC 등)을 import해
	// exec-entrypoint와 run에서 사용할 수 있도록 보장한다.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	// runtime: runtime.Scheme(쿠버네티스 타입 등록표) 타입이 정의된 패키지다.
	"k8s.io/apimachinery/pkg/runtime"
	// utilruntime: 에러 처리 유틸이며, 아래 init()에서 utilruntime.Must를 쓴다.
	// import 경로 앞에 붙은 utilruntime은 별칭(alias)이다.
	// 원래 패키지 이름은 runtime이라 바로 위의 runtime과 이름이 충돌하므로 다른 이름을 지어 준 것이다.
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	// clientgoscheme: Pod, Deployment 같은 쿠버네티스 기본(core) 타입들의 등록 함수를 제공한다.
	// 이 역시 원래 이름이 scheme이라 헷갈리지 않게 별칭을 붙였다.
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	// ctrl: controller-runtime 최상위 패키지이며, 원래 이름이 길어서 ctrl로 짧게 부르는 게 커뮤니티 관례다.
	// Manager 생성, 로거, 시그널 핸들러 등 핵심 진입점이 모두 여기 있다.
	ctrl "sigs.k8s.io/controller-runtime"
	// healthz: liveness/readiness probe용 기본 체크 함수(healthz.Ping)를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	// zap: 구조화 로깅 구현체이며, controller-runtime의 로거로 붙여 쓴다.
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// filters: metrics endpoint를 인증/인가로 보호하는 필터를 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	// metricsserver: Prometheus metrics를 노출하는 HTTP 서버의 설정 타입을 담는다.
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	// webhook: admission webhook 서버의 설정 타입을 담는다.
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들(GPUQuotaPolicy 등)이다.
	// 원래 패키지 이름은 v1이지만 다른 v1들과 헷갈리지 않도록 platformv1이라는 별칭으로 부른다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	// controller: 이 프로젝트의 reconciler들(NodeHealthReconciler 등)이 정의된 내부 패키지다.
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/controller"
	// 아래 줄은 주석이 아니라 kubebuilder scaffold 마커(코드)다.
	// `kubebuilder create api`를 실행하면 이 위치에 새 API 패키지 import가 자동으로 끼워 넣어진다.
	// 손으로 지우거나 번역하면 코드 생성이 깨진다.
	// +kubebuilder:scaffold:imports
)

// var ( ... ) 블록: 패키지 전역 변수 선언을 묶은 것이다.
// 전역으로 두는 이유는 아래 init()과 main() 양쪽에서 같은 값을 써야 하기 때문이다.
var (
	// scheme: 이 프로그램이 다룰 줄 아는 모든 쿠버네티스 타입의 등록표다.
	// client가 API 서버의 JSON을 Go 구조체로 바꾸려면 "이 apiVersion/kind는 이 Go 타입"이라는 대응표가 필요한데 그게 scheme이다.
	// runtime.NewScheme()은 아무것도 등록되지 않은 빈 표를 만들며, 실제 등록은 아래 init()에서 한다.
	scheme = runtime.NewScheme()
	// setupLog: 기동(setup) 단계 전용 로거다.
	// WithName("setup")을 붙이면 이 로거로 찍는 모든 로그에 logger=setup 필드가 따라붙어
	// 나중에 로그를 볼 때 기동 로그와 reconcile 로그를 구분하기 쉬워진다.
	setupLog = ctrl.Log.WithName("setup")
)

// func init(): Go의 특수한 함수이며, 인자도 반환값도 가질 수 없다.
// main()이 시작되기 전에 런타임이 자동으로 딱 한 번 호출해 주므로 우리가 직접 부르지 않는다.
// 전역 변수 초기화가 모두 끝난 뒤에 실행되므로 위의 scheme 변수를 안전하게 사용할 수 있다.
//
// 설계 근거: 타입 등록은 반드시 client가 만들어지기 전에 끝나야 한다.
// init()에 두면 main()의 어떤 코드보다 먼저 실행되는 것이 언어 차원에서 보장되므로 순서 실수를 원천 차단한다.
func init() {
	// clientgoscheme.AddToScheme(scheme): Pod, Node, Deployment 같은 쿠버네티스 기본 타입들을 표에 등록한다.
	// 우리 컨트롤러가 Node를 읽고 Deployment를 만들기 때문에 이 등록이 필요하다.
	//
	// Go 문법 설명:
	//   - utilruntime.Must(err)는 인자로 받은 에러가 nil이 아니면 즉시 panic으로 프로그램을 죽이는 헬퍼다.
	//   - init()은 반환값을 가질 수 없어서 에러를 위로 올릴 방법이 없다.
	//     그래서 "여기서 실패하면 어차피 프로그램이 정상 동작할 수 없다"는 상황에 한해 Must로 즉시 죽인다.
	//     타입 등록 실패는 코드 자체가 잘못된 경우라 재시도해도 소용없으므로 이 관용구가 적절하다.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	// platformv1.AddToScheme(scheme): 우리가 정의한 CRD 타입들(GPUQuotaPolicy, InferenceDeployment 등)을 표에 등록한다.
	// api/v1/groupversion_info.go의 AddToScheme이 바로 이 함수다.
	// 이 줄이 없으면 컨트롤러가 자기 CRD를 읽을 때 "no kind is registered" 에러가 난다.
	utilruntime.Must(platformv1.AddToScheme(scheme))
	// 아래 줄은 주석이 아니라 kubebuilder scaffold 마커(코드)다.
	// 새 API를 만들면 이 자리에 AddToScheme 호출이 자동으로 추가된다.
	// +kubebuilder:scaffold:scheme
}

// main: 프로그램이 시작되는 지점이다.
// package main 안의 func main()은 Go 런타임이 자동으로 호출하며, 이 함수가 반환하면 프로세스가 종료된다.
// 인자도 반환값도 없으며, 커맨드라인 인자는 아래 flag 패키지로, 종료 코드는 os.Exit로 다룬다.
//
// 전체 흐름은 (1)플래그 파싱 → (2)로거 설정 → (3)TLS/metrics/webhook 서버 옵션 구성 →
// (4)Manager 생성 → (5)reconciler 등록 → (6)probe 등록 → (7)Manager 시작(블로킹) 순이다.
//
// 아래 줄은 주석이 아니라 golangci-lint에 주는 지시(directive)다.
// 이 함수의 순환 복잡도(gocyclo)가 높다고 경고하지 말라는 뜻이다.
// 기동 코드는 조건 분기가 많을 수밖에 없고 쪼개면 오히려 읽기 어려워져서 예외로 둔다.
//
// nolint:gocyclo
func main() {
	// 아래는 커맨드라인 플래그 값을 받아 둘 지역 변수들의 선언이다.
	//
	// Go 문법 설명:
	//   - var 이름 타입 은 값을 안 주고 변수만 선언하는 문법이다.
	//     이때 변수는 그 타입의 "제로 값"으로 초기화된다(string은 "", bool은 false, 슬라이스는 nil).
	//   - 실제 값은 곧이어 나오는 flag.StringVar/BoolVar 호출이 채워 준다.
	//     그래서 여기서는 초기값을 주지 않고 선언만 해 둔다.
	//   - var a, b, c string 처럼 같은 타입 변수는 콤마로 이어 한 줄에 선언할 수 있다.

	// metricsAddr: Prometheus metrics를 노출할 주소다("0"이면 비활성).
	var metricsAddr string
	// metrics 서버용 TLS 인증서의 디렉터리/인증서 파일명/키 파일명이다.
	var metricsCertPath, metricsCertName, metricsCertKey string
	// webhook 서버용 TLS 인증서의 디렉터리/인증서 파일명/키 파일명이다.
	var webhookCertPath, webhookCertName, webhookCertKey string
	// enableLeaderElection: leader election을 켤지 여부다.
	var enableLeaderElection bool
	// probeAddr: liveness/readiness probe를 노출할 주소다.
	var probeAddr string
	// secureMetrics: metrics를 HTTPS로 서빙할지 여부다.
	var secureMetrics bool
	// enableHTTP2: HTTP/2를 허용할지 여부다.
	var enableHTTP2 bool
	// tlsOpts: tls.Config를 받아 그 자리에서 고쳐 주는 함수들의 목록이다.
	//
	// Go 문법 설명:
	//   - []T 는 T 타입 값들의 슬라이스(가변 길이 배열)다.
	//   - func(*tls.Config) 는 "tls.Config 포인터 하나를 받고 아무것도 반환하지 않는 함수"라는 타입 자체다.
	//     Go에서 함수는 값이라서 이렇게 타입으로 쓰고 슬라이스에 담을 수 있다.
	//   - 즉 이 변수는 "TLS 설정을 조정하는 함수들의 리스트"이며, 초기값은 nil(빈 슬라이스)이다.
	//   - 이 패턴을 functional options라고 부르며, 설정 변경 로직을 값으로 모아 두었다가 나중에 한꺼번에 적용한다.
	var tlsOpts []func(*tls.Config)

	// 아래는 각 플래그를 flag 패키지에 등록하는 부분이다.
	//
	// Go 문법 설명:
	//   - flag.StringVar(&변수, "플래그이름", 기본값, "도움말") 형태로 쓴다.
	//   - 첫 인자에 & 를 붙여 변수의 주소(포인터)를 넘기는 게 핵심이다.
	//     flag 패키지가 나중에 이 주소에 파싱 결과를 직접 써 넣어야 하므로 값이 아니라 주소가 필요하다.
	//   - 등록만 해서는 값이 채워지지 않고, 아래 flag.Parse()를 부르는 순간 실제 인자를 읽어 대입한다.
	//   - 문자열 끝의 + 는 문자열 이어붙이기(연결) 연산자이며, 긴 도움말을 여러 줄로 나눠 적기 위한 것이다.
	//   - flag.BoolVar도 같은 방식이며 bool 변수를 채운다.
	//     bool 플래그는 --leader-elect 처럼 값 없이 써도 true가 된다.
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	// opts: zap 로거의 설정 값이다.
	// Development: true는 사람이 읽기 좋은 컬러 콘솔 형식과 낮은 로그 레벨(디버그 포함)을 쓰겠다는 뜻이다.
	//
	// Go 문법 설명:
	//   - := 는 "선언과 대입을 동시에" 하는 짧은 변수 선언이며, 타입은 우변에서 자동으로 추론된다.
	//     즉 opts는 zap.Options 타입이 된다.
	//   - zap.Options{...} 는 구조체 리터럴이며, 명시하지 않은 나머지 필드는 각자의 제로 값으로 채워진다.
	opts := zap.Options{
		Development: true,
	}
	// opts.BindFlags(flag.CommandLine): zap 자신의 플래그(--zap-log-level 등)를 flag 패키지에 추가로 등록한다.
	// flag.CommandLine은 flag 패키지가 기본으로 쓰는 전역 플래그 집합이며, 위 StringVar들도 여기에 등록됐다.
	// 같은 집합에 넣어야 아래 flag.Parse() 한 번으로 우리 플래그와 zap 플래그가 함께 파싱된다.
	opts.BindFlags(flag.CommandLine)
	// flag.Parse(): 실제 커맨드라인 인자를 읽어 위에서 등록한 모든 변수에 값을 채운다.
	// 이 줄이 실행되기 전까지 metricsAddr 등은 전부 제로 값이므로, 반드시 플래그를 쓰기 전에 호출해야 한다.
	// 인자가 잘못되면 flag 패키지가 사용법을 출력하고 프로그램을 종료시킨다.
	flag.Parse()

	// ctrl.SetLogger(...): controller-runtime 전체가 사용할 전역 로거를 지정한다.
	// 이 줄을 부르기 전에 찍은 로그는 버려지므로 기동 초반에 반드시 호출해야 한다.
	//
	// Go 문법 설명:
	//   - 함수 호출의 결과를 다른 함수의 인자로 바로 넘기는 중첩 호출이며, 안쪽부터 평가된다.
	//   - zap.UseFlagOptions(&opts)는 파싱된 opts를 반영한 설정을, zap.New(...)는 그 설정으로 만든 로거를 돌려준다.
	//   - &opts 로 주소를 넘기는 이유는 UseFlagOptions의 시그니처가 포인터를 받기 때문이다.
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// disableHTTP2: 주어진 tls.Config가 HTTP/1.1만 협상하도록 고치는 함수다.
	//
	// Go 문법 설명:
	//   - 이름 := func(...) { ... } 는 이름 붙은 익명 함수(클로저)를 변수에 담는 문법이다.
	//     함수 안에서 함수를 정의할 수 있고, 바깥 변수(setupLog)도 그대로 참조할 수 있다.
	//   - 지금 호출하는 게 아니라 "나중에 실행할 동작"을 값으로 만들어 두는 것이다.
	//     실제 호출은 controller-runtime이 TLS 서버를 세울 때 일어난다.
	//   - c.NextProtos = []string{"http/1.1"} 는 ALPN(프로토콜 협상) 후보 목록을 http/1.1 하나로 못 박는다.
	//     []string{"http/1.1"} 은 문자열 하나짜리 슬라이스 리터럴이다.
	//     후보에 h2가 없으면 클라이언트는 HTTP/2로 붙을 수 없다.
	//
	// enable-http2 flag가 false(기본값)면 취약점 때문에 http/2를 비활성화해야 하는데
	// 좀 더 구체적으로 http/2를 끄면 HTTP/2 Stream Cancellation 및 Rapid Reset CVE에
	// 취약해지는 상황을 막을 수 있다.
	// 자세한 내용은 아래를 참고한다:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	// enableHTTP2가 false면(즉 사용자가 --enable-http2를 켜지 않았으면) 위 함수를 옵션 목록에 넣는다.
	//
	// Go 문법 설명:
	//   - ! 는 논리 부정(NOT) 연산자이며, !enableHTTP2 는 "enableHTTP2가 false일 때 참"이라는 뜻이다.
	//   - append(슬라이스, 값)은 슬라이스 끝에 값을 붙인 새 슬라이스를 돌려준다.
	//     Go의 슬라이스는 append가 원본을 바꾸는 게 아니라 결과를 반환하므로
	//     tlsOpts = append(tlsOpts, ...) 처럼 반드시 결과를 다시 대입해야 한다.
	//   - 여기서 붙이는 disableHTTP2는 함수 "값"이며, 아직 실행되지 않고 목록에 담기기만 한다.
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// webhookTLSOpts: webhook 서버에 적용할 TLS 옵션 목록이며, 위에서 만든 tlsOpts를 그대로 쓴다.
	// 변수를 따로 두는 이유는 나중에 webhook만 다른 TLS 정책을 갖게 될 때 이 한 줄만 고치면 되기 때문이다.
	//
	// 초기 webhook TLS option
	webhookTLSOpts := tlsOpts
	// webhookServerOptions: admission webhook 서버를 어떻게 세울지 담는 설정 값이다.
	// 지금은 TLS 옵션만 채우고, 인증서 경로는 플래그가 주어졌을 때만 아래에서 추가로 채운다.
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	// 사용자가 --webhook-cert-path를 준 경우에만 인증서 설정을 덮어쓴다.
	//
	// Go 문법 설명:
	//   - len(문자열)은 그 문자열의 바이트 길이를 준다.
	//     len(s) > 0 은 "빈 문자열이 아니다"를 뜻하는 관용적 표현이다.
	//     이 플래그의 기본값이 ""이므로 이 검사는 곧 "사용자가 값을 줬는가"와 같다.
	//   - setupLog.Info("메시지", "키1", 값1, "키2", 값2, ...) 는 구조화 로깅 방식이다.
	//     첫 인자가 메시지이고 그 뒤는 키와 값이 번갈아 오는 쌍이라서 항상 짝수 개여야 한다.
	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		// 인증서 디렉터리와 파일명을 설정에 반영한다.
		// controller-runtime은 이 경로를 watch해서 인증서가 갱신되면 프로세스 재시작 없이 다시 읽어들인다.
		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	// webhook.NewServer(설정): 위 설정으로 webhook 서버 인스턴스를 만든다.
	// 만들기만 할 뿐 아직 듣기(listen)를 시작하지는 않으며, 아래에서 Manager에 넘기면 Manager가 함께 띄워 준다.
	webhookServer := webhook.NewServer(webhookServerOptions)

	// metricsServerOptions: Prometheus metrics 서버를 어떻게 세울지 담는 설정 값이다.
	//   - BindAddress: 어느 주소에서 들을지이며, 기본값 "0"이면 metrics 서버를 아예 띄우지 않는다.
	//   - SecureServing: true면 HTTPS로 서빙한다.
	//   - TLSOpts: 위에서 모아 둔 TLS 조정 함수 목록이며, HTTP/2 비활성화가 여기에도 똑같이 적용된다.
	//
	// metric endpoint는 'config/default/kustomization.yaml'에서 활성화하고, Metrics option으로 서버를 구성하며,
	// 자세한 내용은 아래를 참고한다:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	// HTTPS로 서빙하는 경우에만 인증/인가 필터를 건다.
	//
	// Go 문법 설명:
	//   - if bool변수 { ... } 처럼 bool 값은 비교 없이 그대로 조건으로 쓴다.
	//     secureMetrics == true 라고 쓰지 않는 게 Go 관례다.
	//   - filters.WithAuthenticationAndAuthorization 뒤에 괄호가 없으므로 호출이 아니라 함수 값 대입이다.
	//     controller-runtime이 metrics 서버를 세울 때 이 함수를 대신 호출해 준다.
	//
	// 설계 근거: metrics에는 테넌트 이름이나 사용량 같은 운영 정보가 담긴다.
	// 인증 없이 열어 두면 클러스터 안 아무 Pod나 이를 읽을 수 있으므로 기본값을 보안 모드로 둔다.
	if secureMetrics {
		// FilterProvider로 metric endpoint를 authn/authz로 보호하여,
		// 인가된 사용자와 서비스 계정만 metric endpoint에 접근하도록 보장하고,
		// RBAC는 'config/rbac/kustomization.yaml'에서 구성하며, 자세한 내용은 아래를 참고한다:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// 인증서를 지정하지 않으면 controller-runtime이 metric 서버용 자체 서명 인증서를 자동 생성하는데,
	// 개발과 테스트에는 편리하지만,
	// 운영 환경에는 권장하지 않는다.
	//
	// TODO(user): certManager를 활성화하려면 아래 line의 주석을 해제한다:
	// - config/default/kustomization.yaml의 [METRICS-WITH-CERTS], cert-manager가 관리하는
	// metric 서버 인증서를 생성해 사용
	// - config/prometheus/kustomization.yaml의 [PROMETHEUS-WITH-CERTS], TLS 인증용
	// webhook 쪽과 같은 패턴이며, 사용자가 --metrics-cert-path를 준 경우에만 인증서 설정을 채운다.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// Manager 생성: 이 프로그램의 심장에 해당하는 객체다.
	// Manager는 client, cache, metrics 서버, webhook 서버, probe 서버, leader election을 한데 묶어
	// 생명주기를 함께 관리해 주는 컨테이너다.
	//
	// Go 문법 설명:
	//   - mgr, err := ... 는 반환값 두 개를 각각 새 변수에 받는 짧은 변수 선언이다.
	//     Go는 값을 여러 개 돌려줄 수 있고, 관례상 마지막 반환값이 error다.
	//   - 에러가 없으면 err은 nil이므로 곧바로 err != nil로 실패 여부를 검사하는 것이 Go의 표준 관용구다.
	//
	// 인자 설명:
	//   - ctrl.GetConfigOrDie(): API 서버 접속 설정(rest.Config)을 찾아 준다.
	//     클러스터 안에서는 ServiceAccount 토큰을, 밖에서는 ~/.kube/config를 자동으로 쓴다.
	//     이름 끝의 OrDie가 뜻하듯 찾지 못하면 에러를 돌려주는 대신 그 자리에서 프로그램을 죽인다.
	//     접속 설정 없이는 아무 일도 할 수 없어 복구할 방법이 없으므로 이 동작이 타당하다.
	//   - ctrl.Options{...}: Manager의 세부 설정을 담은 구조체 리터럴이다.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		// Scheme: 위 init()에서 채운 타입 등록표를 Manager에게 넘긴다.
		// Manager가 만드는 client와 cache가 모두 이 표를 써서 JSON과 Go 구조체를 서로 변환한다.
		Scheme: scheme,
		// Metrics: 앞에서 구성한 metrics 서버 설정이다.
		Metrics: metricsServerOptions,
		// WebhookServer: 앞에서 만든 webhook 서버 인스턴스이며, Manager가 시작/종료를 함께 관리한다.
		WebhookServer: webhookServer,
		// HealthProbeBindAddress: liveness/readiness probe를 서빙할 주소다(기본 :8081).
		// 쿠버네티스가 이 주소를 주기적으로 찔러 Pod가 살아 있는지 판단한다.
		HealthProbeBindAddress: probeAddr,
		// LeaderElection: 켜면 여러 replica 중 리스(lease)를 쥔 하나만 실제로 reconcile을 수행한다.
		//
		// 설계 근거: 컨트롤러는 클러스터 상태를 쓰기 때문에 두 replica가 동시에 같은 리소스를 reconcile하면
		// 서로의 변경을 덮어쓰며 무한 루프를 만들 수 있다.
		// leader election은 "쓰는 주체는 항상 하나"를 보장해 이를 막는다.
		// 나머지 replica는 대기하다가 리더가 죽으면 즉시 넘겨받으므로 가용성도 유지된다.
		LeaderElection: enableLeaderElection,
		// LeaderElectionID: 리스의 이름이며, 이 이름으로 Lease 리소스가 만들어진다.
		// 같은 클러스터의 다른 컨트롤러와 겹치면 서로 리더 자리를 뺏으므로 반드시 고유해야 한다.
		// kubebuilder가 프로젝트 생성 시 만들어 준 무작위 접두사라 손댈 필요가 없다.
		LeaderElectionID: "4b07920a.lkhun9311.github.io",
		// 아래 설명은 지금 주석 처리된 LeaderElectionReleaseOnCancel 옵션에 대한 것이다.
		// 주석을 풀면 켤 수 있으나 기본 scaffold 상태에서는 꺼 둔다.
		//
		// LeaderElectionReleaseOnCancel은 Manager 종료 시 리더가 자발적으로 물러날지 정의하며,
		// 이를 위해서는 Manager가 멈출 때 binary가 즉시 종료돼야 하고,
		// 그렇지 않으면 이 설정은 안전하지 않으며,
		// 이 option을 켜면 새 리더가 LeaseDuration만큼 먼저 기다릴 필요가 없어,
		// 자발적 리더 전환이 크게 빨라진다.
		//
		// 기본 scaffold에서는 manager가 멈춘 직후 프로그램이 종료되므로,
		// 이 option을 켜도 무방하지만,
		// manager 종료 후 정리 작업 같은 어떤 동작을 수행하거나 수행할 예정이라면,
		// 사용이 안전하지 않을 수 있다.
		// LeaderElectionReleaseOnCancel: true,
	})
	// Manager 생성이 실패하면 더 진행할 의미가 없으므로 에러를 남기고 즉시 종료한다.
	//
	// Go 문법 설명:
	//   - setupLog.Error(err, "메시지", 키, 값...) 는 에러를 첫 인자로 받는 구조화 로깅이다.
	//   - os.Exit(1)은 프로세스를 즉시 끝내며, 인자 1은 "비정상 종료"를 뜻하는 종료 코드다(0이 정상).
	//     쿠버네티스는 이 코드를 보고 컨테이너가 실패했다고 판단해 재시작 정책에 따라 다시 띄운다.
	//   - 주의: os.Exit은 defer로 등록해 둔 함수들을 실행하지 않고 곧바로 죽는다.
	//     그래서 정리 작업이 필요한 곳에서는 쓰지 않지만, 여기는 아직 아무것도 시작되지 않은 기동 단계라 안전하다.
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// 여기서부터 reconciler들을 Manager에 등록한다.
	// 등록은 "이 컨트롤러가 어떤 리소스를 watch하고 어떤 함수로 reconcile할지"를 Manager에 알려 주는 일이다.
	// 아직 실행되지는 않으며, 맨 아래 mgr.Start(...)가 불릴 때 한꺼번에 시작된다.
	//
	// Go 문법 설명(아래 네 블록이 모두 같은 패턴이다):
	//   - &controller.XxxReconciler{...} 는 reconciler 구조체 값을 만들고 그 포인터를 얻는다.
	//     SetupWithManager가 포인터 리시버 메서드라서 포인터가 필요하다.
	//   - 바깥의 괄호 (&...{...}) 는 문법상 필수다.
	//     괄호가 없으면 컴파일러가 &controller.X{...}.SetupWithManager 를 어디까지 묶어야 할지 몰라 에러가 난다.
	//   - if err := 호출(); err != nil { ... } 는 호출과 동시에 err을 만들고 즉시 검사하는 Go의 관용구다.
	//     이렇게 만든 err은 이 if 블록 안에서만 살아 있어서 위쪽의 err 변수와 충돌하지 않는다.
	//   - mgr.GetClient()는 Manager가 만든 캐시 기반 client를, mgr.GetScheme()은 위에서 넘긴 scheme을 돌려준다.
	//     reconciler를 직접 만들지 않고 Manager 것을 나눠 쓰는 이유는 cache와 연결을 공유해야 하기 때문이다.
	//     각자 client를 만들면 캐시가 따로 놀아 메모리와 API 서버 부하가 배로 든다.
	//
	// 설계 근거(설계서 Components 절): 네 reconciler가 각각 하나의 CRD를 담당한다.
	//   - NodeHealthReconciler: GPU 노드의 건강 상태를 추적한다.
	//   - GPUQuotaPolicyReconciler: 테넌트별 쿼터 정책을 반영한다.
	//   - InferenceDeploymentReconciler: 추론 서빙 워크로드를 배포한다.
	//   - MLTrainingJobReconciler: 학습 잡을 관리한다.
	// 하나라도 등록에 실패하면 플랫폼이 반쪽만 동작하는 셈이라 부분 기동을 허용하지 않고 즉시 종료한다.
	if err := (&controller.NodeHealthReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		// 어느 컨트롤러가 실패했는지 알 수 있도록 "controller"="nodehealth" 키-값 쌍을 함께 남긴다.
		setupLog.Error(err, "Failed to create controller", "controller", "nodehealth")
		os.Exit(1)
	}
	if err := (&controller.GPUQuotaPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "gpuquotapolicy")
		os.Exit(1)
	}
	if err := (&controller.InferenceDeploymentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "inferencedeployment")
		os.Exit(1)
	}
	if err := (&controller.MLTrainingJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "mltrainingjob")
		os.Exit(1)
	}
	// 아래 줄은 주석이 아니라 kubebuilder scaffold 마커(코드)다.
	// 새 API를 만들면 이 자리에 해당 reconciler의 SetupWithManager 블록이 자동으로 추가된다.
	// +kubebuilder:scaffold:builder

	// probe 등록: 쿠버네티스가 이 Pod의 상태를 물어볼 엔드포인트 두 개를 붙인다.
	//   - healthz(liveness): "프로세스가 살아 있나"이며, 실패하면 쿠버네티스가 컨테이너를 재시작한다.
	//   - readyz(readiness): "트래픽을 받을 준비가 됐나"이며, 실패하면 Service 엔드포인트에서 빠진다.
	//
	// Go 문법 설명:
	//   - healthz.Ping 뒤에 괄호가 없으므로 호출이 아니라 함수 값을 넘기는 것이다.
	//     Manager가 probe 요청을 받을 때마다 이 함수를 대신 호출한다.
	//   - healthz.Ping은 항상 성공을 돌려주는 가장 단순한 체크다.
	//     HTTP 응답이 온다는 것 자체가 프로세스가 살아서 요청을 처리 중이라는 증거이므로 이것만으로 liveness는 충분하다.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	// 여기까지가 준비 단계이고, 이제 실제로 Manager를 돌린다.
	setupLog.Info("Starting manager")
	// mgr.Start(ctx): 등록된 모든 컴포넌트(cache, 컨트롤러, metrics/webhook/probe 서버)를 시작한다.
	// 이 호출은 "블로킹"이다.
	// 즉 인자로 준 context가 취소될 때까지 반환하지 않고 그 자리에 머문다.
	// 따라서 이 줄 아래의 코드는 프로그램이 종료될 때에야 실행된다.
	//
	// Go 문법 설명:
	//   - ctrl.SetupSignalHandler()는 context.Context 하나를 돌려준다.
	//     이 context는 프로세스가 SIGTERM이나 SIGINT(Ctrl+C)를 받으면 자동으로 취소된다.
	//   - 즉 "쿠버네티스가 Pod를 내릴 때 Manager도 스스로 멈춘다"는 graceful shutdown 배선이 이 한 줄이다.
	//     신호를 두 번 받으면(예: 종료가 지연될 때) 즉시 강제 종료된다.
	//
	// 설계 근거: reconcile 도중 프로세스가 갑자기 죽으면 리소스가 중간 상태로 남을 수 있다.
	// 시그널을 context 취소로 바꿔 전달하면 진행 중인 작업이 정리될 시간을 벌 수 있다.
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		// 정상 종료(시그널에 의한 취소)에서는 nil이 돌아오므로 여기에 오지 않는다.
		// 여기에 왔다면 예기치 못한 실패이므로 비정상 종료 코드로 끝낸다.
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
	// main()이 반환하면 프로세스가 정상(종료 코드 0)으로 끝난다.
}
