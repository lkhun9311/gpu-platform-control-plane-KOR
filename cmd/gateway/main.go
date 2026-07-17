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
// 이 파일은 서빙 게이트웨이 바이너리의 진입점이며, cmd/main.go(controller-manager)와는 별개의 실행 파일로 빌드된다.
// 디렉터리가 다르면(cmd/ 와 cmd/gateway/) 같은 package main이라도 서로 다른 바이너리가 된다.
//
// 설계 근거(설계서 Components 절): 게이트웨이는 데이터 플레인이고 controller-manager는 컨트롤 플레인이다.
// 게이트웨이는 사용자 추론 요청을 받아 인증하고, 테넌트를 식별하고, rate limit을 적용한 뒤 백엔드로 넘긴다.
// 이 경로는 요청마다 지연시간이 곧바로 사용자에게 보이는 실시간 경로다.
// 반면 컨트롤러의 reconcile은 초 단위로 느려도 되는 비동기 경로다.
// 두 워크로드를 한 프로세스에 두면 무거운 reconcile 루프가 추론 요청의 꼬리 지연시간을 밀어 올린다.
// 또 게이트웨이는 트래픽에 맞춰 여러 replica로 늘려야 하지만 컨트롤러는 leader election으로 하나만 활성이어야 해서
// 확장 단위 자체가 다르다.
// 그래서 바이너리를 분리하고 CRD 타입 정의(api/v1)만 공유한다.
//
// gateway command, tenant 인식 OpenAI 호환 서빙 gateway 구동
package main

// import 블록: 이 파일이 사용하는 외부 패키지들을 선언한다.
import (
	// net/http: HTTP 서버와 클라이언트를 담은 표준 패키지다.
	// 여기서는 http.ListenAndServe로 서버를 띄운다.
	"net/http"
	// os: 운영체제와 상호작용하는 표준 패키지이며, os.Exit와 os.Getenv에 쓴다.
	"os"

	// platformv1: 우리 프로젝트가 정의한 CRD 타입들이며, 원래 패키지 이름 v1 대신 별칭으로 부른다.
	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	// gateway: 게이트웨이의 실제 구현(Server, NewCache, 레이트 리밋 등)이 있는 내부 패키지다.
	// 이 main.go는 배선(wiring)만 담당하고 로직은 전부 이 패키지에 있다.
	"github.com/lkhun9311/gpu-mlops-platform-control-plane/internal/gateway"
	// runtime: runtime.Scheme(쿠버네티스 타입 등록표) 타입이 정의된 패키지다.
	"k8s.io/apimachinery/pkg/runtime"
	// clientgoscheme: Secret, Pod 같은 쿠버네티스 기본(core) 타입들의 등록 함수를 제공한다.
	// 원래 이름이 scheme이라 헷갈리지 않게 별칭을 붙였다.
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	// ctrl: controller-runtime 최상위 패키지이며, 관례상 ctrl로 짧게 부른다.
	// 게이트웨이는 컨트롤러가 아니지만 로거, 시그널 핸들러, 접속 설정 로딩은 그대로 재사용한다.
	ctrl "sigs.k8s.io/controller-runtime"
	// zap: 구조화 로깅 구현체다.
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// main: 프로그램이 시작되는 지점이다.
// package main 안의 func main()은 Go 런타임이 자동으로 호출하며, 이 함수가 반환하면 프로세스가 종료된다.
//
// 전체 흐름은 (1)로거/시그널 설정 → (2)scheme 등록 → (3)cache 생성 → (4)Server 조립 →
// (5)cache와 metrics 서버를 고루틴으로 시작 → (6)메인 API 서버 시작(블로킹) 순이다.
func main() {
	// 게이트웨이 전역 로거를 설정한다.
	// zap.UseDevMode(true)는 사람이 읽기 좋은 개발용 콘솔 형식을 쓰겠다는 뜻이다.
	// cmd/main.go와 달리 여기서는 플래그로 로그 설정을 받지 않고 코드에 고정한다.
	// 게이트웨이는 설정 표면을 최소로 유지하고 필요한 값은 아래처럼 환경변수로만 받기 때문이다.
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	// log: 이 바이너리 전용 로거이며, WithName("gateway")로 모든 로그에 logger=gateway 필드가 붙는다.
	//
	// Go 문법 설명:
	//   - := 는 선언과 대입을 동시에 하는 짧은 변수 선언이며, 타입은 우변에서 추론된다.
	//   - 함수 안에서만 쓸 수 있고, 패키지 전역에서는 var를 써야 한다.
	log := ctrl.Log.WithName("gateway")
	// ctx: 프로세스 종료 신호를 전파하는 context다.
	// ctrl.SetupSignalHandler()가 돌려주는 이 context는 SIGTERM이나 SIGINT(Ctrl+C)를 받으면 자동으로 취소된다.
	// 아래에서 cache와 cache sync 대기에 이 ctx를 넘기므로, 종료 신호 하나로 그것들이 함께 멈춘다.
	ctx := ctrl.SetupSignalHandler()

	// scheme: 이 프로그램이 다룰 줄 아는 쿠버네티스 타입의 등록표다.
	// cmd/main.go는 이를 전역 변수와 init()으로 만들었지만, 여기서는 main() 안의 지역 변수로 둔다.
	// 게이트웨이는 이 값을 쓰는 곳이 아래 NewCache 한 군데뿐이라 굳이 전역으로 노출할 이유가 없다.
	// 변수의 유효 범위는 좁을수록 좋다는 원칙에 따른 것이다.
	//
	// gateway가 읽는 core 및 platform type 등록
	scheme := runtime.NewScheme()
	// core 타입 등록: 게이트웨이는 API 키가 담긴 Secret을 읽으므로 이 등록이 필요하다.
	//
	// Go 문법 설명:
	//   - if err := 호출(); err != nil { ... } 는 호출과 동시에 err을 만들고 즉시 검사하는 관용구다.
	//     이렇게 만든 err은 이 if 블록 안에서만 유효해서 아래 다른 err과 이름이 겹쳐도 문제없다.
	//   - cmd/main.go의 init()은 utilruntime.Must로 panic을 냈지만 여기서는 직접 검사한다.
	//     init()과 달리 main() 안에서는 로그를 남기고 os.Exit로 깔끔히 끝낼 수 있어서
	//     panic 스택트레이스보다 읽기 좋은 실패 메시지를 줄 수 있다.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "register client-go scheme")
		// os.Exit(1)은 프로세스를 즉시 끝내며, 1은 비정상 종료를 뜻하는 종료 코드다(0이 정상).
		os.Exit(1)
	}
	// CRD 타입 등록: 게이트웨이는 GPUQuotaPolicy를 읽어 테넌트별 rate limit을 적용하므로 이 등록이 필요하다.
	if err := platformv1.AddToScheme(scheme); err != nil {
		log.Error(err, "register platform scheme")
		os.Exit(1)
	}

	// cfg: API 서버 접속 설정(rest.Config)이다.
	// ctrl.GetConfigOrDie()는 클러스터 안에서는 ServiceAccount 토큰을, 밖에서는 ~/.kube/config를 자동으로 쓴다.
	// 이름 끝의 OrDie가 뜻하듯 찾지 못하면 에러를 돌려주는 대신 그 자리에서 프로그램을 죽인다.
	cfg := ctrl.GetConfigOrDie()
	// namespace를 cache 생성 전에 확정한다.
	//
	// 왜 여기로 끌어올렸는가:
	// 이 값은 아래 Server의 Namespace 필드에도 쓰이지만, NewCache가 Secret 감시 범위를 가두는 데에도 필요하다.
	// 두 곳이 서로 다른 namespace를 보면 게이트웨이는 A를 감시하면서 B에서 Secret을 찾게 되어
	// 모든 요청이 401로 떨어진다. 한 변수에서 갈라 쓰면 그런 어긋남이 생길 수 없다.
	namespace := envOr("GATEWAY_NAMESPACE", "default")
	// gateway.NewCache(...)는 반환값을 세 개 돌려준다.
	//   - ca: cache 인스턴스이며, 아래에서 고루틴으로 Start를 호출해 돌린다.
	//   - cl: 그 cache를 통해 읽는 client이며, Server에 넣어 준다.
	//   - err: 실패 시의 에러다.
	//
	// Go 문법 설명: Go는 값을 여러 개 반환할 수 있고, 관례상 마지막 반환값을 error로 둔다.
	//
	// 설계 근거(설계서 Components 절): 게이트웨이는 요청마다 정책과 Secret을 읽어야 한다.
	// 매 요청 API 서버를 호출하면 지연시간이 밀리초 단위로 늘고 API 서버에도 부하가 걸린다.
	// cache는 watch로 변경을 미리 받아 메모리에 들고 있어서 읽기를 사실상 0에 가까운 비용으로 만든다.
	ca, cl, err := gateway.NewCache(ctx, cfg, scheme, namespace)
	if err != nil {
		log.Error(err, "build cache")
		os.Exit(1)
	}

	// s: 게이트웨이 서버 인스턴스다.
	// 실제 HTTP 핸들러 로직은 internal/gateway 패키지에 있고, 여기서는 의존성만 주입해 조립한다.
	//
	// Go 문법 설명:
	//   - &gateway.Server{...} 는 구조체 값을 만든 뒤 그 주소(포인터)를 얻는 표현이다.
	//     포인터여야 아래 고루틴의 s.MarkReady()와 메인 흐름의 s.Handler()가 같은 인스턴스를 공유한다.
	//     값으로 두면 고루틴이 복사본에 준비 완료 표시를 해서 메인 쪽은 영영 준비되지 않는다.
	//
	// 필드 설명:
	//   - Client: 위에서 만든 cache 기반 client다.
	//   - Namespace: API 키 Secret이 있는 네임스페이스다.
	//   - APIKeySecret: API 키가 담긴 Secret의 이름이다.
	//
	// 설계 근거: 이 두 값은 플래그가 아니라 환경변수로 받는다.
	// 쿠버네티스 Deployment의 env 필드로 주입하는 것이 컨테이너 환경의 표준 관례이고,
	// 값이 바뀌어도 이미지나 커맨드 인자를 건드리지 않고 매니페스트만 고치면 되기 때문이다.
	s := &gateway.Server{
		Client:       cl,
		Namespace:    namespace,
		APIKeySecret: envOr("GATEWAY_API_KEY_SECRET", "gateway-api-keys"),
	}
	// tenant별 token bucket 등록부를 켠다.
	//
	// 왜 조립 단계에서 하는가:
	// bucketRegistry는 gateway 패키지의 비공개 타입이라 여기서 직접 만들 수 없다.
	// 그래서 gateway 쪽이 공개 메서드 하나를 열어 두고, main은 "속도 제한을 켠다"는 의사만 밝힌다.
	// 이 호출을 빠뜨리면 buckets가 nil인 채로 요청을 받게 되므로, gateway 쪽에서 그 경우를 막아 둔다.
	s.InitRateLimiter()

	// 아래부터 고루틴 세 개를 띄운다.
	//
	// Go 문법 설명:
	//   - go func() { ... }() 는 익명 함수를 만들어 즉시 "고루틴"으로 실행하는 관용구다.
	//     맨 끝의 () 가 호출이고, 앞의 go 키워드가 "이 호출을 별도의 경량 스레드에서 돌려라"는 뜻이다.
	//   - go로 띄우면 호출한 쪽은 끝나기를 기다리지 않고 다음 줄로 바로 넘어간다.
	//   - 익명 함수는 바깥 변수(ca, ctx, log, s)를 그대로 참조할 수 있으며 이를 클로저라고 부른다.
	//   - 고루틴이 필요한 이유는 ca.Start와 http.ListenAndServe가 모두 "블로킹" 함수이기 때문이다.
	//     즉 한 번 부르면 끝날 때까지 반환하지 않아서, 순서대로 부르면 뒤의 것이 영영 시작되지 않는다.
	//     그래서 마지막 하나만 메인 흐름에 두고 나머지는 고루틴으로 돌린다.

	// 고루틴 1: cache를 시작한다.
	// ca.Start(ctx)는 watch 연결을 열고 ctx가 취소될 때까지 계속 돌며 변경을 받아 메모리를 갱신한다.
	// 종료 신호로 ctx가 취소되면 정상적으로 반환하므로 이때는 에러 로그가 남지 않는다.
	//
	// cache 시작, sync 완료되면 readiness 전환
	go func() {
		if err := ca.Start(ctx); err != nil {
			log.Error(err, "cache stopped")
		}
	}()
	// 고루틴 2: cache의 최초 sync가 끝나기를 기다렸다가 서버를 "준비 완료"로 표시한다.
	// WaitForCacheSync(ctx)는 초기 목록을 다 받을 때까지 블로킹하며, 성공하면 true를, ctx가 먼저 취소되면 false를 준다.
	// false면 종료 중이라는 뜻이므로 아무것도 하지 않고 그냥 끝난다.
	//
	// 설계 근거: cache가 채워지기 전에 요청을 받으면 정책을 못 찾아 정상 테넌트를 403으로 거절하게 된다.
	// 그래서 sync가 끝나기 전에는 readiness를 false로 두어 쿠버네티스가 트래픽을 보내지 않게 막는다.
	// 이 대기를 메인 흐름에 두지 않고 고루틴으로 분리한 이유는 그동안에도 :8081의 readiness 엔드포인트가
	// 응답은 해야("아직 준비 안 됨"이라고) 쿠버네티스가 Pod를 죽이지 않고 기다려 주기 때문이다.
	go func() {
		if ca.WaitForCacheSync(ctx) { // cache sync 대기 후 준비 완료 표시
			// MarkReady()는 Server 내부의 준비 완료 플래그를 켠다.
			// 이 고루틴과 요청 처리 고루틴이 동시에 그 플래그를 건드리므로 Server 쪽에서 동시성 안전하게 구현되어 있다.
			s.MarkReady()
			log.Info("cache synced; gateway ready")
		}
	}()

	// 고루틴 3: 운영용 엔드포인트(metrics, readiness)를 :8081에서 서빙한다.
	//
	// 설계 근거: 포트를 둘로 나눈다.
	//   - :8080은 사용자 추론 트래픽(OpenAI 호환 API)이며 외부에 노출된다.
	//   - :8081은 metrics와 probe이며 클러스터 내부 전용이다.
	// 나누는 이유는 사용량 정보가 담긴 metrics가 사용자에게 노출되면 안 되고,
	// 사용자 트래픽이 폭주해 :8080이 포화돼도 probe는 계속 응답해 Pod가 불필요하게 재시작되지 않아야 하기 때문이다.
	//
	// Go 문법 설명:
	//   - http.ListenAndServe(주소, 핸들러)는 그 주소에서 듣기 시작하고 요청이 올 때마다 핸들러를 부른다.
	//     정상 상황에서는 반환하지 않으며, 반환했다면 그 자체가 무언가 잘못됐다는 뜻이라 항상 에러를 돌려준다.
	//   - s.MetricsHandler()는 그 서버가 쓸 핸들러(http.Handler)를 만들어 돌려주는 메서드다.
	//
	// metric/readiness는 :8081, OpenAI 호환 API는 :8080에서 서빙
	go func() {
		if err := http.ListenAndServe(":8081", s.MetricsHandler()); err != nil {
			log.Error(err, "metrics server stopped")
		}
	}()
	// 이제 메인 흐름에서 사용자 트래픽 서버를 시작한다.
	// 이 줄부터가 게이트웨이의 본업이므로 고루틴이 아니라 메인 고루틴에 그대로 둔다.
	log.Info("serving", "addr", ":8080")
	// 이 호출은 블로킹이라 프로세스가 사는 동안 여기서 머문다.
	// main()이 반환하면 Go 런타임이 남은 고루틴을 기다리지 않고 프로세스를 끝내므로,
	// 이렇게 메인 흐름을 붙잡아 두는 것이 곧 프로세스를 살려 두는 방법이다.
	if err := http.ListenAndServe(":8080", s.Handler()); err != nil {
		// 여기 도달했다는 것은 서버가 멈췄다는 뜻이며, 게이트웨이는 더 이상 제 역할을 못 한다.
		// 조용히 살아 있으면 쿠버네티스가 정상으로 오해하므로 비정상 종료 코드로 끝내 재시작을 유도한다.
		log.Error(err, "serving stopped")
		os.Exit(1)
	}
}

// envOr: key라는 이름의 환경변수를 읽어 돌려주고, 값이 없거나 비어 있으면 기본값 def를 돌려준다.
//
// Go 문법 설명:
//   - 리시버가 없는 일반 함수다(특정 타입에 붙지 않음).
//   - (key, def string) 처럼 같은 타입 인자는 타입을 한 번만 적어도 된다.
//   - 소문자로 시작하는 이름이라 이 패키지 안에서만 보이는 "비공개" 함수다.
//   - os.Getenv(key)는 환경변수가 설정되지 않았으면 빈 문자열("")을 돌려준다.
//     즉 "설정 안 됨"과 "빈 값으로 설정됨"을 구분하지 않는데, 여기서는 둘 다 기본값을 쓰는 게 맞아 문제되지 않는다.
//     (구분이 필요하면 os.LookupEnv를 쓴다.)
//   - if v := ...; v != "" 는 호출과 동시에 v를 만들고 즉시 검사하는 관용구이며, v는 이 if 안에서만 유효하다.
//
// key의 환경변수 값 반환, 미설정 시 def 사용
func envOr(key, def string) string {
	// 환경변수에 실제 값이 있으면 그걸 그대로 쓴다.
	if v := os.Getenv(key); v != "" {
		return v
	}
	// 값이 없으면 호출자가 준 기본값으로 대체한다.
	// 기본값을 두는 덕분에 로컬 개발 시 환경변수를 하나도 설정하지 않아도 바로 실행된다.
	return def
}
