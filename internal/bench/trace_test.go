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

package bench

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func twoTenantParams(seed int64) TraceParams {
	return TraceParams{
		Seed:       seed,
		DurationMs: 60_000,
		RatePerSec: 20,
		Tenants: []TenantSpec{
			{Tenant: "premium-1", Weight: 1, PromptLenChars: 200, MaxOutputTokens: 64, IsNoisy: false},
			{Tenant: "standard-noisy", Weight: 1, PromptLenChars: 40_000, MaxOutputTokens: 16, IsNoisy: true},
		},
	}
}

var _ = Describe("GenerateTrace", func() {
	It("is deterministic: the same seed yields an identical trace and checksum", func() {
		a, err := GenerateTrace(twoTenantParams(42))
		Expect(err).NotTo(HaveOccurred())
		b, err := GenerateTrace(twoTenantParams(42))
		Expect(err).NotTo(HaveOccurred())
		Expect(a).To(Equal(b))

		var bufA, bufB bytes.Buffer
		Expect(WriteTrace(&bufA, a)).To(Succeed())
		Expect(WriteTrace(&bufB, b)).To(Succeed())
		Expect(Checksum(bufA.Bytes())).To(Equal(Checksum(bufB.Bytes())))
	})

	It("produces a different trace for a different seed", func() {
		a, err := GenerateTrace(twoTenantParams(1))
		Expect(err).NotTo(HaveOccurred())
		b, err := GenerateTrace(twoTenantParams(2))
		Expect(err).NotTo(HaveOccurred())
		var bufA, bufB bytes.Buffer
		Expect(WriteTrace(&bufA, a)).To(Succeed())
		Expect(WriteTrace(&bufB, b)).To(Succeed())
		Expect(Checksum(bufA.Bytes())).NotTo(Equal(Checksum(bufB.Bytes())))
	})

	It("emits non-decreasing arrival offsets and stable indices", func() {
		rows, err := GenerateTrace(twoTenantParams(7))
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).ToNot(BeEmpty())
		for i := range rows {
			Expect(rows[i].Index).To(Equal(i))
			if i > 0 {
				Expect(rows[i].OffsetMs).To(BeNumerically(">=", rows[i-1].OffsetMs))
			}
		}
	})

	It("honors the tenant weight ratio within sampling tolerance", func() {
		params := twoTenantParams(3)
		// Skew the split 3:1 so the observed ratio is unambiguous.
		params.Tenants[0].Weight = 3
		params.Tenants[1].Weight = 1
		params.RatePerSec = 200 // more arrivals, tighter law of large numbers
		rows, err := GenerateTrace(params)
		Expect(err).NotTo(HaveOccurred())

		var premium int
		for _, r := range rows {
			if r.Tenant == "premium-1" {
				premium++
			}
		}
		frac := float64(premium) / float64(len(rows))
		Expect(frac).To(BeNumerically("~", 0.75, 0.06))
	})

	It("rejects invalid params", func() {
		_, err := GenerateTrace(TraceParams{DurationMs: 0, RatePerSec: 1, Tenants: []TenantSpec{{Tenant: "a", Weight: 1}}})
		Expect(err).To(HaveOccurred())
		_, err = GenerateTrace(TraceParams{DurationMs: 1000, RatePerSec: 0, Tenants: []TenantSpec{{Tenant: "a", Weight: 1}}})
		Expect(err).To(HaveOccurred())
		_, err = GenerateTrace(TraceParams{DurationMs: 1000, RatePerSec: 1})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("WriteTrace and ReadTrace", func() {
	It("round-trips a trace byte-for-byte through JSON Lines", func() {
		rows, err := GenerateTrace(twoTenantParams(11))
		Expect(err).NotTo(HaveOccurred())

		var buf bytes.Buffer
		Expect(WriteTrace(&buf, rows)).To(Succeed())
		got, err := ReadTrace(bytes.NewReader(buf.Bytes()))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(rows))
	})

	It("rejects a trace whose offsets are not non-decreasing", func() {
		out := []byte(`{"index":0,"offsetMs":100,"tenant":"a","promptLenChars":10,"maxOutputTokens":8,"isNoisy":false}` + "\n" +
			`{"index":1,"offsetMs":50,"tenant":"a","promptLenChars":10,"maxOutputTokens":8,"isNoisy":false}` + "\n")
		_, err := ReadTrace(bytes.NewReader(out))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("non-decreasing"))
	})
})
