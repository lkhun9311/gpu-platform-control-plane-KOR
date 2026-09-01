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
	"net/http"
	"net/http/httptest"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("resolveTenant", func() {
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
		tenant, ok, _ := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse())
		Expect(tenant).To(BeEmpty())
	})

	It("returns ok=false for an unknown bearer key", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer unknown")
		tenant, ok, _ := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse())
		Expect(tenant).To(BeEmpty())
	})

	It("resolves a known bearer key to its tenant", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer k1")
		tenant, ok, _ := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeTrue())
		Expect(tenant).To(Equal("team-vision"))
	})

	It("resolves a known bearer key case-insensitively", func() {
		s := newServer()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "bearer k1")
		tenant, ok, _ := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeTrue())
		Expect(tenant).To(Equal("team-vision"))
	})
})

// A Secret the gateway cannot read is not a caller whose key is wrong.
//
// Both used to collapse into ok=false and answer 401, so RBAC revoked, the object deleted or the apiserver
// briefly unreachable told every tenant at once that their credential was invalid. That sends an operator to
// rotate keys while the fault is on the cluster, and a fleet-wide 401 is the shape of an outage, not of an
// auth failure.
//
// Mutation that turns this red: return ok=false with a nil error when the Get fails.
var _ = Describe("an unreadable api-keys secret", func() {
	It("is reported as unavailable rather than unauthorized", func() {
		s := &Server{
			Client:       failingReader{},
			Namespace:    "ns",
			APIKeySecret: "api-keys",
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer some-real-key")

		_, ok, err := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse())
		Expect(err).To(MatchError(ErrKeyStoreUnavailable),
			"a cluster fault was reported as an invalid credential")
	})

	// The control: a key that is genuinely absent from a readable Secret is still an ordinary rejection with
	// no error, or the change has turned every bad key into an outage.
	It("still rejects an unknown key without an error", func() {
		c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gateway-api-keys", Namespace: "gw"},
			Data:       map[string][]byte{"k1": []byte("team-vision")},
		}).Build()
		s := &Server{Client: c, Namespace: "gw", APIKeySecret: "gateway-api-keys"}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer nope")

		_, ok, err := s.resolveTenant(context.Background(), r)
		Expect(ok).To(BeFalse())
		Expect(err).NotTo(HaveOccurred())
	})
})

// And end to end, because the resolver returning an error is only half of it: the handler has to answer
// something other than 401.
//
// Mutation that turns this red: drop the err branch from the handler's tenant step, so an unreadable Secret
// falls through to Unauthorized again.
var _ = Describe("a request whose key store cannot be read", func() {
	It("is answered 503 rather than 401", func() {
		s := &Server{Client: failingReader{}, Namespace: "gw", APIKeySecret: "gateway-api-keys"}
		s.InitRateLimiter()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`))
		r.Header.Set("Authorization", "Bearer some-real-key")
		rr := httptest.NewRecorder()

		s.Handler().ServeHTTP(rr, r)

		Expect(rr.Code).To(Equal(http.StatusServiceUnavailable),
			"a cluster fault told the caller their credential was invalid")
		Expect(rr.Body.String()).To(ContainSubstring("gateway_unavailable"))
	})
})

// failingReader fails every Get, standing in for a Secret the gateway has lost access to.
type failingReader struct {
	client.Client
}

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("secrets is forbidden")
}
