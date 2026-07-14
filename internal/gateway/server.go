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

// Package gateway는 tenant를 인식하는 OpenAI 호환 서빙 gateway 구현이다
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// InferenceDeployment.spec.model.name에 대한 cache field index key,
// routing은 이 index로 요청된 model을 해당 Service로 해석하며 CR field selector나 요청별 apiserver 호출이 없다.
const ModelNameIndex = ".spec.model.name"

// gateway의 공유 의존성과 HTTP handler를 보유
type Server struct {
	// scope가 지정된 cache에서 InferenceDeployment, GPUQuotaPolicy, api-keys Secret을 읽음
	Client client.Client
	// Namespace와 APIKeySecret은 tenant 해석에 쓰는 api-keys Secret의 위치를 지정
	Namespace    string
	APIKeySecret string
	// cache가 동기화되면 true로 뒤집히며 readiness를 gating
	ready atomic.Bool
}

// gateway가 서빙 가능한 상태임을 표시
func (s *Server) markReady() { s.ready.Store(true) }

// cache의 첫 동기화 이후 binary가 readiness를 뒤집기 위한 exported 진입점
func (s *Server) MarkReady() { s.markReady() }

// cache가 동기화된 후에만 200 반환, 아니면 503을 반환해 Pod가 Service endpoint에서 빠지도록 함
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "cache not synced", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// :8080에서 서빙하는 mux,
// 이후 작업에서 POST /v1/chat/completions를 추가할 예정이다.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// :8081에서 관측성을 담당하는 mux,
// 이후 작업에서 /metrics를 추가할 예정이다.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// gateway의 읽기 집합에 대한 scope cache와 cache를 읽고 apiserver에 쓰는 위임 client를 생성하고,
// routing에 쓰는 model-name field indexer를 등록한다.
func NewCache(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (cache.Cache, client.Client, error) {
	ca, err := cache.New(cfg, cache.Options{
		Scheme:           scheme,
		DefaultTransform: cache.TransformStripManagedFields(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("new cache: %w", err)
	}
	// routing 조회를 위해 InferenceDeployment를 그것이 서빙하는 model 이름으로 index
	if err := ca.IndexField(ctx, &platformv1.InferenceDeployment{}, ModelNameIndex, func(o client.Object) []string {
		return []string{o.(*platformv1.InferenceDeployment).Spec.Model.Name}
	}); err != nil {
		return nil, nil, fmt.Errorf("index %s: %w", ModelNameIndex, err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: ca}})
	if err != nil {
		return nil, nil, fmt.Errorf("new delegating client: %w", err)
	}
	return ca, cl, nil
}
