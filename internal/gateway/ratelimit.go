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
	"context"
	"errors"
	"sync"

	// 직접 구현 대신 Google이 관리하는 검증된 token bucket limiter를 쓴다 (설계서 §Components)
	"golang.org/x/time/rate"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// "이 tenant에 해당하는 GPUQuotaPolicy가 하나도 없음"을 나타내는 sentinel error
//
// 설계 근거(설계서 §Identity model): tenant에 정책이 0개면 gateway는 403을 반환해야 하고,
// 이 error는 "읽기 실패"가 아니라 "정책 미프로비저닝"이라는 정상적 결과 신호이므로,
// 상위 handler가 이걸 받아 403으로 변환한다.
var ErrNoPolicy = errors.New("no GPUQuotaPolicy for tenant")

// 주어진 tenant를 담당하는 GPUQuotaPolicy 하나를 찾아 돌려주며,
// 없으면 위의 ErrNoPolicy를 반환한다.
//
// 설계 근거(설계서 §Identity model): GPUQuotaPolicy는 cluster scope이고 같은 spec.tenant를
// 가진 정책이 2개 이상 존재하는 것을 막지 못하며, 그런 경우 limiter 상태가 비결정적이 되면 안 되므로,
// "가장 오래된(creationTimestamp가 이른) 정책이 이기고 시간이 같으면 이름 오름차순"이라는,
// 결정론적 규칙으로 딱 하나를 고른다.
func (s *Server) policyForTenant(ctx context.Context, tenant string) (*platformv1.GPUQuotaPolicy, error) {
	var list platformv1.GPUQuotaPolicyList
	if err := s.Client.List(ctx, &list); err != nil {
		// 읽기 자체가 실패하면(예: API server 오류) 그 error를 그대로 위로 올린다
		return nil, err
	}

	// 조건에 맞는 정책 중 가장 오래된 하나를 고른다
	var oldest *platformv1.GPUQuotaPolicy
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.Tenant != tenant {
			continue
		}
		if oldest == nil || olderPolicy(p, oldest) {
			oldest = p
		}
	}

	if oldest == nil {
		// 이 tenant엔 정책이 하나도 없다
		return nil, ErrNoPolicy
	}
	return oldest, nil
}

// a가 b보다 우선(더 오래됨)인지 답하며,
// 생성 시각이 더 이르면 우선이고 시각이 완전히 같으면 이름 사전순으로 앞선 쪽이 우선이다.
func olderPolicy(a, b *platformv1.GPUQuotaPolicy) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		// 시각이 같으면 이름 오름차순으로 tie-break
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// tenant별로 token bucket limiter를 하나씩 보관하는 저장소
//
// 설계 근거(설계서 §Components): map[tenant]*rate.Limiter 구조로 단일 replica라 in-memory map으로
// 충분하고(분산 bucket은 out of scope), 여러 요청이 동시에 이 map을 읽고 쓰므로 Mutex로 보호한다.
type bucketRegistry struct {
	// buckets map을 동시 접근으로부터 보호하는 잠금 (Go의 내장 map은 동시 쓰기에 안전하지 않다)
	mu sync.Mutex
	// tenant 이름 → 해당 tenant의 bucket
	buckets map[string]*trackedBucket
}

// limiter 하나와 그 limiter를 만들 때 쓴 정책 설정(rpm, burst)을 함께 보관하며,
// 설정값을 같이 들고 있어야 나중에 정책이 바뀌었는지(rpm/burst가 달라졌는지) 비교해서,
// limiter를 갱신할지 판단할 수 있다.
type trackedBucket struct {
	limiter *rate.Limiter
	// 현재 반영 중인 requestsPerMinute (정책 변경 감지용)
	rpm int32
	// 현재 반영 중인 burst (정책 변경 감지용)
	burst int32
}

// 비어 있는 bucketRegistry를 만들어 돌려주는 생성자,
// buckets map을 미리 make로 초기화한다 (nil map에 값을 넣으면 panic).
func newBucketRegistry() *bucketRegistry {
	return &bucketRegistry{buckets: make(map[string]*trackedBucket)}
}

// tenant가 지금 요청을 보내도 되는지 판단하고 허용이면 token 하나를 소비하며,
// true면 통과, false면 한도 초과다 (상위 handler가 429 rate_limited로 변환).
//
// 설계 근거(설계서 §Components, §Request flow 4단계):
//   - rateLimit이 nil이면 무제한 tenant라 항상 허용
//   - 초당 속도 = requestsPerMinute / 60 (분당 값을 그대로 초당에 넣으면 60배 빨라지는 bug, /60 필수)
//   - 정책이 바뀌면 limiter를 새로 만들지 않고 SetLimit/SetBurst로 갱신하며, 그동안 쌓인 token 상태를 보존
func (b *bucketRegistry) Allow(tenant string, rl *platformv1.GPUQuotaRateLimit) bool {
	// rl이 nil이면 이 tenant엔 gateway 속도 제한이 없으므로 무조건 통과
	if rl == nil {
		return true
	}

	// 분당 정책값을 초당 보충 속도로 변환 (정수 나눗셈으로 소수점이 잘리지 않도록 먼저 float64로 변환)
	limit := rate.Limit(float64(rl.RequestsPerMinute) / 60.0)
	burst := int(rl.Burst)

	b.mu.Lock()
	tb, ok := b.buckets[tenant]
	if !ok {
		// 처음 보는 tenant면 limiter를 새로 만들어 등록한다 (생성 직후 bucket은 가득 차 있다)
		tb = &trackedBucket{
			limiter: rate.NewLimiter(limit, burst),
			rpm:     rl.RequestsPerMinute,
			burst:   rl.Burst,
		}
		b.buckets[tenant] = tb
	} else if tb.rpm != rl.RequestsPerMinute || tb.burst != rl.Burst {
		// 정책값이 달라졌으면 limiter를 버리지 않고 설정만 갱신한다,
		// 통째로 새로 만들면 bucket이 다시 가득 차서 설정을 바꾼 tenant에게 공짜 burst를 주게 된다.
		tb.limiter.SetLimit(limit)
		tb.limiter.SetBurst(burst)
		tb.rpm = rl.RequestsPerMinute
		tb.burst = rl.Burst
	}
	// 잠금 구간을 짧게 유지하려고 limiter pointer만 빼두고 map 잠금을 푼다
	limiter := tb.limiter
	b.mu.Unlock()

	// rate.Limiter는 자체적으로 동시성 안전하므로 잠금 밖에서 호출한다,
	// token이 있으면 하나 소비하며 true, 없으면 false를 반환한다.
	return limiter.Allow()
}
