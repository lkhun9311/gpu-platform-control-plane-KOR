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
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// gateway package의 Ginkgo suite 등록
func TestGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Suite")
}

var _ = Describe("readiness", func() {
	It("is 503 before cache sync and 200 after", func() {
		s := &Server{}
		rr := httptest.NewRecorder()
		s.readyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		Expect(rr.Code).To(Equal(http.StatusServiceUnavailable)) // cache 동기화 전이라 503
		s.markReady()
		rr2 := httptest.NewRecorder()
		s.readyz(rr2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		Expect(rr2.Code).To(Equal(http.StatusOK)) // markReady 이후 200
	})
})
