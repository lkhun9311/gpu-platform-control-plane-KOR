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
	// context: 요청의 취소/타임아웃 신호를 전달하는 표준 타입이다.
	"context"
	// errors: 아래 ErrNoRoute 센티넬 에러를 만드는 데 쓴다.
	"errors"
	// fmt: 에러 래핑(%w)과 URL 문자열 조립에 쓴다.
	"fmt"
	// net/url: 조립한 문자열을 구조화된 URL 값으로 파싱한다.
	// 문자열을 그대로 들고 다니지 않고 *url.URL로 돌려주는 이유는, 프록시(httputil.ReverseProxy)가
	// 이 타입을 그대로 받기 때문이다.
	"net/url"

	// client: List에 넘길 조회 옵션(InNamespace, MatchingFields)을 제공한다.
	"sigs.k8s.io/controller-runtime/pkg/client"
	// log: ctx에 실려 온 로거를 꺼내 쓴다.
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// ErrNoRoute: "요청한 model을 서빙하는 InferenceDeployment가 하나도 없다"를 나타내는 센티넬 에러다.
//
// Go 문법 설명:
//   - ratelimit.go의 ErrNoPolicy와 같은 방식이다.
//     전역에 값 하나를 만들어 두면 호출한 쪽에서 errors.Is(err, ErrNoRoute)로 정확히 비교할 수 있다.
//
// 설계 근거(설계서 Error codes 절): 알 수 없는 model 요청은 404가 되어야 한다.
// 이 에러는 "조회 실패"가 아니라 "그런 model이 없음"이라는 정상적 결과 신호이므로,
// 상위 핸들러가 이걸 받아 404로 변환한다.
var ErrNoRoute = errors.New("no inferencedeployment for model")

// servingPort: InferenceDeployment가 서빙하는 포트를 돌려준다.
//
// 왜 0을 따로 처리하는가:
// spec.port에는 +kubebuilder:default=8080 마커가 붙어 있어서, API server를 거쳐 저장된 객체는
// 값이 비어 있어도 8080으로 채워져 들어온다. 그래서 운영 경로에서는 0이 나올 수 없다.
// 하지만 테스트의 fake client는 defaulting을 적용하지 않아 0이 그대로 남는다.
// 포트 0으로 URL을 만들면 프록시가 붙지 못하므로, 코드 쪽에서도 같은 기본값을 보장해 둔다.
func servingPort(infd *platformv1.InferenceDeployment) int32 {
	if infd.Spec.Port == 0 {
		return 8080
	}
	return infd.Spec.Port
}

// olderInfD: a가 b보다 우선(더 오래됨)인지 true/false로 답하는 헬퍼다.
// 규칙은 생성 시각이 더 이르면 우선이고, 시각이 완전히 같으면 이름 사전순으로 앞선 쪽이 우선이다.
//
// 왜 시각만으로 비교하면 안 되는가:
// CreationTimestamp는 초 단위 정밀도라서, 같은 초에 만들어진 두 객체는 시각이 완전히 같아진다.
// 그때 시각만 비교하면 양방향 비교가 모두 false가 되어 어느 쪽이 뽑힐지가 비결정적이 된다.
// 그러면 같은 요청이 매번 다른 backend로 갈 수 있는데, 이는 아래 backendFor의 설계 근거가 금지하는 상태다.
// ratelimit.go의 olderPolicy가 GPUQuotaPolicy에 대해 이미 같은 규칙을 쓴다.
func olderInfD(a, b *platformv1.InferenceDeployment) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		// 시각이 같으니 이름으로 tie-break 한다.
		// 이름은 같은 namespace 안에서 유일하므로 이 비교는 항상 한쪽으로 결정된다.
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// backendFor: 요청된 model을 그 model을 서빙하는 InferenceDeployment의 Service URL로 해석한다.
// 해당하는 것이 없으면 위의 ErrNoRoute를 반환한다.
//
// Go 문법 설명:
//   - 리시버 (s *Server): 이 메서드는 게이트웨이 Server 인스턴스에 붙는다.
//   - 소문자 backendFor: 패키지 안에서만 쓰는 비공개 메서드다.
//   - 반환 타입 (*url.URL, error): 관례대로 마지막을 error로 두고, 에러가 없으면 nil을 넣는다.
//
// 설계 근거(설계서 Components 절): 조회는 ModelNameIndex 필드 인덱스로 캐시에서 한다.
// 인덱스를 쓰므로 CR field selector도, 요청마다 API server를 부르는 일도 없다.
// 조회 범위를 정책의 TargetNamespace로 한정하는 것이 tenant 격리의 핵심이다.
// 서로 다른 tenant가 같은 model 이름을 쓰는 일은 흔하므로, namespace를 빠뜨리면 남의 backend로 요청이 샌다.
//
// 설계 근거(설계서 Identity model 절): 같은 model을 서빙하는 InferenceDeployment가 둘 이상인 것을
// 막을 수단이 없다. 그런 경우에도 요청이 backend 사이를 오가면 안 되므로,
// "가장 오래된 것이 이기고, 시각이 같으면 이름 오름차순"이라는 결정론적 규칙으로 딱 하나를 고르고 경고를 남긴다.
func (s *Server) backendFor(ctx context.Context, policy *platformv1.GPUQuotaPolicy, model string) (*url.URL, error) {
	// var list ... : InferenceDeployment 목록을 담을 빈 변수를 선언한다.
	var list platformv1.InferenceDeploymentList
	// client.InNamespace(...)로 정책이 지정한 namespace로 범위를 좁히고,
	// client.MatchingFields{ModelNameIndex: model}로 미리 걸어 둔 인덱스를 조회한다.
	// 이 인덱스는 server.go의 NewCache가 .spec.model.name에 대해 등록한다.
	if err := s.Client.List(ctx, &list,
		client.InNamespace(policy.Spec.TargetNamespace),
		client.MatchingFields{ModelNameIndex: model}); err != nil {
		// 조회 자체가 실패한 경우다.
		// %w로 감싸면 호출한 쪽이 errors.Is/As로 원래 에러를 그대로 들여다볼 수 있다.
		return nil, fmt.Errorf("list inferencedeployments: %w", err)
	}
	// 하나도 없으면 이 model을 서빙하는 곳이 없다는 뜻이다.
	if len(list.Items) == 0 {
		return nil, ErrNoRoute
	}

	// oldest: 지금까지 본 것 중 가장 오래된 것을 가리킬 포인터다.
	// 항목이 최소 하나는 있음을 위에서 확인했으므로 첫 번째로 시작한다.
	oldest := &list.Items[0]
	// for i := range list.Items : 인덱스로 훑는 이유는 &list.Items[i]에서 원본 요소의 주소가 필요하기 때문이다.
	for i := range list.Items {
		if olderInfD(&list.Items[i], oldest) {
			oldest = &list.Items[i]
		}
	}

	// 둘 이상이면 운영자가 알아야 할 상태다.
	// 요청은 결정론적으로 처리되므로 실패시키지 않고 경고만 남긴다.
	if len(list.Items) > 1 {
		log.FromContext(ctx).Info("multiple InferenceDeployments for model; using oldest",
			"model", model, "chosen", oldest.Name)
	}

	// Service 이름은 InferenceDeployment 이름과 같고 같은 namespace에 있다(inferencedeployment_controller.go가 그렇게 만든다).
	// 그래서 클러스터 내부 주소는 http://<name>.<namespace>.svc:<port> 형태가 된다.
	return url.Parse(fmt.Sprintf("http://%s.%s.svc:%d", oldest.Name, oldest.Namespace, servingPort(oldest)))
}
