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
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// 요청의 Bearer API key를 api-keys Secret을 통해 tenant로 해석하고,
// key가 없거나 알 수 없는 key면 ok=false를 반환한다.
func (s *Server) resolveTenant(ctx context.Context, r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	// 첫 공백 기준으로 scheme과 자격증명 분리
	scheme, credential, found := strings.Cut(h, " ")
	// HTTP 인증 scheme은 RFC 7235상 대소문자 구분 없음, 그래서 EqualFold로 비교
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	key := strings.TrimSpace(credential)
	if key == "" {
		return "", false
	}
	var sec corev1.Secret
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.APIKeySecret, Namespace: s.Namespace}, &sec); err != nil {
		return "", false
	}
	tenant, ok := sec.Data[key]
	return string(tenant), ok && len(tenant) > 0 // 값이 비어있지 않아야 유효한 tenant
}
