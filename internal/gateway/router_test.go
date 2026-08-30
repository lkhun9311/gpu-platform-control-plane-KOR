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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1 "github.com/lkhun9311/gpu-mlops-platform-control-plane/api/v1"
)

// newInfD builds an InferenceDeployment for tests.
func newInfD(name, ns, model string, port int32, created time.Time) *platformv1.InferenceDeployment {
	return &platformv1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: platformv1.InferenceDeploymentSpec{
			Model: platformv1.InferenceModel{Name: model},
			Port:  port,
		},
	}
}

// newRouterClient builds a fake client with ModelNameIndex registered.
//
// NewCache installs that index in production; the fake client does not use that cache, so the tests register the same key and extractor to make MatchingFields lookups behave as they do in production.
func newRouterClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(newSchemeForTest()).
		WithIndex(&platformv1.InferenceDeployment{}, ModelNameIndex, func(o client.Object) []string {
			return []string{o.(*platformv1.InferenceDeployment).Spec.Model.Name}
		}).
		WithObjects(objs...).
		Build()
}

// policyFor builds the minimal policy backendFor needs: it only reads TargetNamespace.
//
// The ns parameter stays despite unparam flagging it as always "vision": it is what makes each call site show which namespace the policy scopes to.
//
// The tenant-isolation spec in particular puts the InferenceDeployment in "nlp" and asks with a "vision" policy to expect ErrNoRoute, and that contrast is only legible with both namespaces side by side at the call site.
//
// Dropping the parameter hides it inside the helper and leaves that spec unreadable.
//
// nolint:unparam
func policyFor(ns string) *platformv1.GPUQuotaPolicy {
	return &platformv1.GPUQuotaPolicy{
		Spec: platformv1.GPUQuotaPolicySpec{TargetNamespace: ns},
	}
}

var _ = Describe("backendFor", func() {
	var (
		ctx = context.Background()
		now = time.Now()
	)

	It("returns ErrNoRoute when no InferenceDeployment serves the model", func() {
		// Guards against returning a nil URL for an unknown model, which would panic the proxy.
		//
		// The design spec Error codes section requires a 404 here, so the error must be distinguishable.
		s := &Server{Client: newRouterClient()}
		_, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).To(MatchError(ErrNoRoute))
	})

	It("resolves the model to its Service URL", func() {
		infd := newInfD("llama", "vision", "llama-3-8b", 9000, now)
		s := &Server{Client: newRouterClient(infd)}
		got, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.URL.String()).To(Equal("http://llama.vision.svc:9000"))
	})

	It("populates every BackendRef field for a routed model", func() {
		// The scraper (a later task) needs namespace/name/port to build the metrics URL and manage lifecycle, which a bare *url.URL cannot provide.
		infd := newInfD("llama", "vision", "llama-3-8b", 9000, now)
		s := &Server{Client: newRouterClient(infd)}
		got, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Namespace).To(Equal("vision"))
		Expect(got.Name).To(Equal("llama"))
		Expect(got.Port).To(Equal(int32(9000)))
		Expect(got.Model).To(Equal("llama-3-8b"))
		Expect(got.URL.String()).To(Equal("http://llama.vision.svc:9000"))
	})

	It("defaults the port to 8080 when spec.port is unset", func() {
		// The fake client applies no defaulting, so spec.port stays 0 where the API server would have set 8080.
		//
		// A URL on port 0 is unroutable, so the code must supply the same default.
		infd := newInfD("llama", "vision", "llama-3-8b", 0, now)
		s := &Server{Client: newRouterClient(infd)}
		got, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Port).To(Equal(int32(8080)))
		Expect(got.URL.String()).To(Equal("http://llama.vision.svc:8080"))
	})

	It("uses the older InferenceDeployment when more than one serves the model", func() {
		older := newInfD("llama-old", "vision", "llama-3-8b", 8080, now.Add(-time.Hour))
		newer := newInfD("llama-new", "vision", "llama-3-8b", 8080, now)
		// newer is added first, so a pass proves the choice follows creation time, not insertion order.
		s := &Server{Client: newRouterClient(newer, older)}
		got, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.URL.String()).To(Equal("http://llama-old.vision.svc:8080"))
	})

	It("picks the lexicographically smaller name when creation timestamps tie", func() {
		// This alone does not guard the non-determinism regression: the fake client returns the list name-sorted, so the tie-break winner is already first and a timestamp-only rule would pass here too.
		//
		// The olderInfD specs below pin the rule itself.
		same := now
		b := newInfD("llama-b", "vision", "llama-3-8b", 8080, same)
		a := newInfD("llama-a", "vision", "llama-3-8b", 8080, same)
		s := &Server{Client: newRouterClient(b, a)}
		got, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.URL.String()).To(Equal("http://llama-a.vision.svc:8080"))
	})

	It("ignores an InferenceDeployment outside the policy's target namespace", func() {
		// Guards the tenant boundary: dropping the namespace filter would leak requests to another tenant's backend, and distinct tenants commonly serve the same model name.
		other := newInfD("llama", "nlp", "llama-3-8b", 8080, now)
		s := &Server{Client: newRouterClient(other)}
		_, err := s.headBackendFor(ctx, policyFor("vision"), "llama-3-8b")
		Expect(err).To(MatchError(ErrNoRoute))
	})
})

var _ = Describe("olderInfD", func() {
	now := time.Now()

	It("prefers the earlier creation timestamp", func() {
		older := newInfD("z-name", "vision", "m", 8080, now.Add(-time.Hour))
		newer := newInfD("a-name", "vision", "m", 8080, now)
		// The later name still loses, so timestamp remains the primary key.
		Expect(olderInfD(older, newer)).To(BeTrue())
		Expect(olderInfD(newer, older)).To(BeFalse())
	})

	It("breaks an exact timestamp tie by ascending name", func() {
		// Guards the non-determinism regression: creationTimestamp has second granularity, so objects created in the same second compare equal and a timestamp-only rule leaves them unordered.
		//
		// Sorting an unordered pair then depends on input order, and the cache guarantees none.
		//
		// The same request could reach different backends, which the design spec Identity model section forbids.
		//
		// Names are unique within a namespace, so the tie-break always decides.
		same := now
		a := newInfD("llama-a", "vision", "m", 8080, same)
		b := newInfD("llama-b", "vision", "m", 8080, same)
		Expect(olderInfD(a, b)).To(BeTrue())
		// The reverse must be false; both false would mean the pair is unordered.
		Expect(olderInfD(b, a)).To(BeFalse())
	})
})

// headBackendFor returns only the backend a request routes to first.
//
// It lives in the test file because only tests call it: a production function nothing in production reaches
// is a claim about the code that is not true of the build.
//
// It exists for the specs that predate fallback, and it keeps them honest rather than merely compiling: they
// pin which deployment wins when several serve one model, and that choice must not drift now that the losers
// are kept instead of discarded. The head is the whole of the old behaviour.
func (s *Server) headBackendFor(ctx context.Context, policy *platformv1.GPUQuotaPolicy, model string) (*BackendRef, error) {
	refs, err := s.backendsFor(ctx, policy, model)
	if err != nil {
		return nil, err
	}
	return refs[0], nil
}
