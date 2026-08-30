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
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestGateway registers the Ginkgo suite for the gateway package.
func TestGateway(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway Suite")
}

var _ = Describe("readiness", func() {
	It("is 503 before cache sync and 200 after", func() {
		s := &Server{}
		rr := httptest.NewRecorder()
		s.readyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		Expect(rr.Code).To(Equal(http.StatusServiceUnavailable))
		s.markReady()
		rr2 := httptest.NewRecorder()
		s.readyz(rr2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		Expect(rr2.Code).To(Equal(http.StatusOK))
	})
})

// The point of admission_mode_active is that it is present when nothing else about admission is, so the two
// things worth asserting are that it appears for a mode which publishes no other series at all ("off"), and
// that switching modes leaves exactly one series behind rather than accumulating one per mode ever set — a
// reader seeing two modes at 1 has no way to tell which one is running.
var _ = Describe("admission mode series", func() {
	It("reports the installed mode and never leaves a stale one behind", func() {
		s := &Server{}

		s.SetAdmitter(AdmissionOff, nil)
		Expect(testutil.ToFloat64(admissionModeActive.WithLabelValues(string(AdmissionOff)))).To(Equal(1.0))
		Expect(testutil.CollectAndCount(admissionModeActive)).To(Equal(1))

		s.SetAdmitter(AdmissionKVAware, nil)
		Expect(testutil.ToFloat64(admissionModeActive.WithLabelValues(string(AdmissionKVAware)))).To(Equal(1.0))
		Expect(testutil.CollectAndCount(admissionModeActive)).To(Equal(1))
	})
})
