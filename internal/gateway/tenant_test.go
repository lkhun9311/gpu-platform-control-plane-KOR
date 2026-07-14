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
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("resolveTenant", func() {
	// k1 key를 team-vision tenant로 매핑한 Secret을 담은 fake client 준비
	newServer := func() *Server {
		c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gateway-api-keys", Namespace: "gw"},
			Data:       map[string][]byte{"k1": []byte("team-vision")},
		}).Build()
		return &Server{Client: c, Namespace: "gw", APIKeySecret: "gateway-api-keys"}
	}

	It("returns ok=false when the Authorization header is missing", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse()) // Authorization header 없으면 실패
		Expect(tenant).To(BeEmpty())
	})

	It("returns ok=false for an unknown bearer key", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer unknown")
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse()) // Secret에 없는 key라 실패
		Expect(tenant).To(BeEmpty())
	})

	It("resolves a known bearer key to its tenant", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer k1")
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeTrue())
		Expect(tenant).To(Equal("team-vision"))
	})

	It("resolves a known bearer key case-insensitively", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "bearer k1") // 소문자 bearer도 통과하는지 확인
		tenant, ok := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeTrue())
		Expect(tenant).To(Equal("team-vision"))
	})
})
