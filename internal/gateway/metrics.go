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
	// net/http: 아래 metricsHTTPHandler가 돌려주는 Handler 타입이 여기서 온다.
	"net/http"

	// prometheus: metric 타입(Counter, Histogram)과 등록부(Registry)를 제공하는 표준 클라이언트 라이브러리다.
	"github.com/prometheus/client_golang/prometheus"
	// promauto: metric을 만들면서 등록까지 한 번에 해주는 보조 패키지다.
	// prometheus.NewCounterVec + MustRegister 두 단계를 한 줄로 줄여준다.
	"github.com/prometheus/client_golang/prometheus/promauto"
	// promhttp: 등록된 metric들을 Prometheus가 긁어갈 수 있는 HTTP handler로 노출한다.
	"github.com/prometheus/client_golang/prometheus/promhttp"
	// metrics: controller-runtime의 전역 metric 등록부다.
	// 이 프로젝트의 컨트롤러들이 이미 이 등록부를 쓰므로, gateway도 같은 곳에 등록해 규약을 맞춘다.
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metric 이름의 공통 접두사다.
//
// 이 문자열은 취향이 아니라 계약이다(docs/05 Minimum metrics 절).
// 문서가 네 시계열의 이름을 gpuaas_gateway_ 로 못박아 두었고, 대시보드와 알림 규칙이 그 문자열로 질의한다.
// 접두사를 바꾸면 게이트웨이는 멀쩡히 동작하는데 그래프만 비어 버리며, 코드 어디에서도 그 사실이 드러나지 않는다.
// 그래서 proxy_test.go의 "metric names" 명세가 문서의 이름을 그대로 적어 두고 이 상수를 고정한다.
//
// 접두사를 두는 이유 자체는 Prometheus 관례다.
// 하나로 이 컴포넌트의 모든 시계열을 골라낼 수 있고, 컨트롤러가 같은 등록부에 올리는 metric과 이름이 충돌하지 않는다.
const metricPrefix = "gpuaas_gateway_"

// 아래 네 개가 gateway가 노출하는 시계열이다.
//
// Go 문법 설명:
//   - var ( ... ) 블록은 패키지 수준 변수를 한 번에 선언한다.
//     이 변수들은 프로그램 시작 시 딱 한 번 초기화되며(= metric이 등록부에 등록되며),
//     이후 모든 요청 goroutine이 같은 인스턴스를 공유한다.
//   - CounterVec/HistogramVec의 Vec은 "라벨별로 갈라지는 묶음"이라는 뜻이다.
//     WithLabelValues(...)로 라벨 값을 지정하면 그 조합에 해당하는 개별 metric이 나온다.
//   - Counter는 오직 증가만 하는 값이고, Histogram은 관측값을 구간별로 세는 분포다.
//   - promauto.With(등록부)를 쓰면 만들면서 그 등록부에 자동 등록된다.
//     등록에 실패하면(같은 이름 중복 등) panic이 나는데, 이는 시작 시점에 즉시 드러나는 편이 낫기 때문이다.
//
// 라벨 선택의 근거(설계서 Observability 절): tenant와 model을 라벨로 두면
// "어느 tenant가 어느 model을 얼마나 쓰는가"를 그대로 집계할 수 있고, 이는 과금과 용량 계획의 입력이다.
// 반대로 라벨에 request id나 API key처럼 값의 가짓수가 무한한 것을 넣으면 시계열이 폭발하므로 절대 넣지 않는다.
var (
	// requests: 처리한 요청 수를 tenant, model, 상태 코드별로 센다.
	//
	// code를 라벨에 두는 이유: 성공과 실패를 한 시계열에서 분리해 볼 수 있어야
	// "429가 튀는가", "502가 늘었는가" 같은 질문에 바로 답할 수 있다.
	requests = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "requests_total",
			Help: "Total gateway requests by tenant, model, and response code.",
		},
		[]string{"tenant", "model", "code"},
	)

	// requestDuration: 요청 처리 시간의 분포를 초 단위로 관측한다.
	//
	// 왜 Counter가 아니라 Histogram인가:
	// 평균만으로는 "대부분 빠른데 일부가 아주 느린" 상황을 볼 수 없다.
	// Histogram이어야 p95/p99 같은 분위수를 구할 수 있고, 지연 SLO는 항상 분위수로 정의된다.
	//
	// 버킷 선택의 근거: DefBuckets는 최대 10초까지만 다루는 일반 웹 요청용 기본값이라 LLM 추론에 맞지 않는다.
	// 토큰 생성은 수십 초가 예사이므로 상한을 120초까지 늘려 잡았다.
	// 상한을 낮게 두면 느린 요청이 전부 +Inf 버킷에 뭉쳐 p99를 계산할 수 없다.
	requestDuration = promauto.With(metrics.Registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricPrefix + "request_duration_seconds",
			Help:    "Gateway request duration in seconds by tenant and model.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"tenant", "model"},
	)

	// rateLimited: 속도 제한으로 거절한 요청 수를 tenant별로 센다.
	//
	// 왜 requests의 code=429와 별도로 두는가:
	// 값 자체는 겹치지만, 이 metric은 "어느 tenant가 자기 몫을 초과하고 있는가"라는 질문 전용이다.
	// 라벨이 tenant 하나뿐이라 model 차원을 합칠 필요 없이 바로 알림 규칙으로 쓸 수 있다.
	//
	// model 라벨이 없는 이유: 속도 제한은 model이 파싱되기 전에 판정된다(아래 chatCompletions의 순서 참고).
	// 그 시점에는 model을 아직 모르므로 라벨로 남길 수가 없다.
	rateLimited = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "rate_limited_total",
			Help: "Total requests rejected by the per-tenant rate limiter.",
		},
		[]string{"tenant"},
	)

	// upstreamErrors: 업스트림에 닿지 못한 횟수를 tenant, model별로 센다.
	//
	// 왜 따로 세는가(설계서 Observability 절): 502/504는 게이트웨이의 잘못이 아니라 backend의 문제다.
	// 이 둘을 구분해야 "게이트웨이를 봐야 하는가, 모델 서버를 봐야 하는가"를 즉시 판단할 수 있다.
	upstreamErrors = promauto.With(metrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "upstream_errors_total",
			Help: "Total upstream connection failures by tenant and model.",
		},
		[]string{"tenant", "model"},
	)
)

// metricsHTTPHandler: 등록된 metric들을 노출하는 HTTP handler를 만든다.
//
// Go 문법 설명: promhttp.HandlerFor(등록부, 옵션)은 그 등록부의 metric만 노출하는 handler를 만든다.
// 인자 없는 promhttp.Handler()를 쓰면 controller-runtime 등록부가 아니라
// prometheus의 전역 기본 등록부를 노출하게 되어, 위에서 등록한 시계열이 하나도 보이지 않는다.
func metricsHTTPHandler() http.Handler {
	return promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})
}
