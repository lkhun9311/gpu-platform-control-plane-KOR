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
	"math"

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

// ladderRungParams is a two-tenant trace under the independent-arrivals model, shaped like one rung of the
// M5-c ladder: a premium rate that climbs and a contender that is supposed to stay exactly where it was.
func ladderRungParams(seed int64, premiumRate, contenderRate float64) TraceParams {
	return TraceParams{
		Seed:       seed,
		DurationMs: 60_000,
		Tenants: []TenantSpec{
			{Tenant: "premium-1", RatePerSec: premiumRate, PromptLenChars: 200, MaxOutputTokens: 64, IsNoisy: false},
			{Tenant: "standard-noisy", RatePerSec: contenderRate, PromptLenChars: 40_000, MaxOutputTokens: 16, IsNoisy: true},
		},
	}
}

// rowsOf returns one tenant's rows with Index cleared.
//
// Index is a position in the merged trace, so it legitimately moves when a DIFFERENT tenant gets more
// arrivals. What must not move is the schedule, and that is everything else on the row.
func rowsOf(rows []TraceRow, tenant string) []TraceRow {
	var out []TraceRow
	for _, r := range rows {
		if r.Tenant == tenant {
			r.Index = 0
			out = append(out, r)
		}
	}
	return out
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

var _ = Describe("GenerateTrace under the weighted model", func() {
	// The weighted model is frozen, not merely tested.
	//
	// Every manifest this repository has ever written pins its trace's checksum, and paid evidence is
	// verified against it. A refactor that moves this byte string does not fail somewhere visible; it
	// silently invalidates the runs that have already been bought. So the value is written down here, and
	// changing the weighted generator has to be a decision someone makes against this line.
	It("still produces the exact trace the paid manifests were checksummed against", func() {
		rows, err := GenerateTrace(twoTenantParams(42))
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1199))

		var buf bytes.Buffer
		Expect(WriteTrace(&buf, rows)).To(Succeed())
		Expect(Checksum(buf.Bytes())).To(Equal(
			"cb132cc21fbbac6c3ddbddb0cd3b4d6b26882d77a6d94d8826b578d33e248fab"))
	})
})

var _ = Describe("GenerateTrace under the independent-arrivals model", func() {
	// This is the defect the M5-c ladder recorded against itself and the reason the model exists.
	//
	// The ladder solved a contender WEIGHT for every rung so the contender would be offered 139 requests
	// give or take two while the premium rate climbed. The count came out right and the schedule did not:
	// changing the total rate changes every gap, so each rung faced a different contender. A rung-to-rung
	// comparison therefore moved the premium rate AND the contention it was measured against.
	It("holds a contender's schedule byte-identical while another tenant's rate quadruples", func() {
		slow, err := GenerateTrace(ladderRungParams(9, 1.16, 0.5))
		Expect(err).NotTo(HaveOccurred())
		fast, err := GenerateTrace(ladderRungParams(9, 4.61, 0.5))
		Expect(err).NotTo(HaveOccurred())

		Expect(rowsOf(fast, "standard-noisy")).To(Equal(rowsOf(slow, "standard-noisy")))
		// The premium side must actually have moved, or the assertion above is vacuous.
		Expect(len(rowsOf(fast, "premium-1"))).To(BeNumerically(">", 2*len(rowsOf(slow, "premium-1"))))
	})

	It("holds a tenant's schedule byte-identical when another tenant is added or removed", func() {
		two, err := GenerateTrace(ladderRungParams(9, 1.16, 0.5))
		Expect(err).NotTo(HaveOccurred())

		withProbe := ladderRungParams(9, 1.16, 0.5)
		withProbe.Tenants = append(withProbe.Tenants,
			TenantSpec{Tenant: "probe-over", RatePerSec: 0.05, PromptLenChars: 16_384, MaxOutputTokens: 8, IsNoisy: true})
		three, err := GenerateTrace(withProbe)
		Expect(err).NotTo(HaveOccurred())

		Expect(rowsOf(three, "standard-noisy")).To(Equal(rowsOf(two, "standard-noisy")))
		Expect(rowsOf(three, "premium-1")).To(Equal(rowsOf(two, "premium-1")))
		Expect(rowsOf(three, "probe-over")).ToNot(BeEmpty())
	})

	// Appending a tenant cannot tell a name-keyed stream from a position-keyed one, because nobody before it
	// moves. Removing the FIRST tenant shifts the contender from position 1 to 0, which is where they differ.
	It("holds a tenant's schedule byte-identical when a tenant listed before it is removed", func() {
		two, err := GenerateTrace(ladderRungParams(9, 1.16, 0.5))
		Expect(err).NotTo(HaveOccurred())

		alone := ladderRungParams(9, 1.16, 0.5)
		alone.Tenants = alone.Tenants[1:]
		one, err := GenerateTrace(alone)
		Expect(err).NotTo(HaveOccurred())

		Expect(rowsOf(one, "standard-noisy")).To(Equal(rowsOf(two, "standard-noisy")))
	})

	It("gives two tenants at the same rate their own schedules rather than one shared one", func() {
		rows, err := GenerateTrace(ladderRungParams(9, 1, 1))
		Expect(err).NotTo(HaveOccurred())

		offsets := func(tenant string) []int64 {
			var out []int64
			for _, r := range rowsOf(rows, tenant) {
				out = append(out, r.OffsetMs)
			}
			return out
		}
		Expect(offsets("premium-1")).ToNot(Equal(offsets("standard-noisy")))
	})

	// Rates this high put two tenants on the same millisecond, which is the only place the tie-break decides
	// anything. At ladder rates a tie is rare enough that this spec would pass without one.
	It("does not let the order the tenants were passed in reach the trace, even on a shared millisecond", func() {
		forward, err := GenerateTrace(ladderRungParams(9, 50, 50))
		Expect(err).NotTo(HaveOccurred())
		ties := 0
		for i := 1; i < len(forward); i++ {
			if forward[i].OffsetMs == forward[i-1].OffsetMs && forward[i].Tenant != forward[i-1].Tenant {
				ties++
			}
		}
		Expect(ties).To(BeNumerically(">", 0), "no cross-tenant tie, so the order assertion below is vacuous")

		reversed := ladderRungParams(9, 50, 50)
		reversed.Tenants[0], reversed.Tenants[1] = reversed.Tenants[1], reversed.Tenants[0]
		back, err := GenerateTrace(reversed)
		Expect(err).NotTo(HaveOccurred())
		Expect(back).To(Equal(forward))
	})

	It("emits non-decreasing offsets and stable indices across the merge", func() {
		rows, err := GenerateTrace(ladderRungParams(4, 2.31, 0.5))
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).ToNot(BeEmpty())
		for i := range rows {
			Expect(rows[i].Index).To(Equal(i))
			if i > 0 {
				Expect(rows[i].OffsetMs).To(BeNumerically(">=", rows[i-1].OffsetMs))
			}
		}
	})

	It("gives each tenant its own rate rather than a share of a total", func() {
		rows, err := GenerateTrace(ladderRungParams(5, 4, 1))
		Expect(err).NotTo(HaveOccurred())
		// 60 s at 4/s and 1/s. Poisson counts, so the tolerance is wide enough not to flake and narrow
		// enough to catch a rate read as a weight, which would split one total four ways instead.
		Expect(len(rowsOf(rows, "premium-1"))).To(BeNumerically("~", 240, 50))
		Expect(len(rowsOf(rows, "standard-noisy"))).To(BeNumerically("~", 60, 25))
	})

	It("is deterministic for a fixed seed and different for a different one", func() {
		a, err := GenerateTrace(ladderRungParams(1, 2, 0.5))
		Expect(err).NotTo(HaveOccurred())
		again, err := GenerateTrace(ladderRungParams(1, 2, 0.5))
		Expect(err).NotTo(HaveOccurred())
		other, err := GenerateTrace(ladderRungParams(2, 2, 0.5))
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(a))
		Expect(other).ToNot(Equal(a))
	})
})

var _ = Describe("GenerateTrace refusing an unstated arrival process", func() {
	It("refuses a trace that mixes weights and per-tenant rates", func() {
		p := ladderRungParams(1, 2, 0.5)
		p.Tenants[1].RatePerSec = 0
		p.Tenants[1].Weight = 1
		_, err := GenerateTrace(p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mix arrival models"))
	})

	It("refuses per-tenant rates alongside a total rate", func() {
		p := ladderRungParams(1, 2, 0.5)
		p.RatePerSec = 10
		_, err := GenerateTrace(p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("total rate is meaningless"))
	})

	It("refuses a tenant left without a rate when the others have one", func() {
		p := ladderRungParams(1, 2, 0.5)
		p.Tenants = append(p.Tenants, TenantSpec{Tenant: "probe-over", PromptLenChars: 16_384})
		_, err := GenerateTrace(p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("every tenant needs its own rate"))
	})

	// An infinite rate is a zero gap, so a generator that accepts one loops until the machine runs out of memory.
	It("refuses an infinite per-tenant rate", func() {
		p := ladderRungParams(1, 2, 0.5)
		p.Tenants[0].RatePerSec = math.Inf(1)
		_, err := GenerateTrace(p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must be finite"))
	})

	It("refuses an infinite total rate under the weighted model", func() {
		p := twoTenantParams(42)
		p.RatePerSec = math.Inf(1)
		_, err := GenerateTrace(p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("positive and finite"))
	})

	// A finite rate can still be one no replay could offer, and generating it is how a test takes the machine down.
	It("refuses a finite rate that would outgrow any replayable trace, under either model", func() {
		weighted := twoTenantParams(1)
		weighted.RatePerSec = 1e12
		_, err := GenerateTrace(weighted)
		Expect(err).To(MatchError(ContainSubstring("rows before reaching durationMs")))

		_, err = GenerateTrace(ladderRungParams(1, 1e12, 0.5))
		Expect(err).To(MatchError(ContainSubstring("rows before reaching durationMs")))
	})

	It("refuses a duplicated tenant name, which would share one stream", func() {
		p := ladderRungParams(1, 2, 0.5)
		p.Tenants[1].Tenant = "premium-1"
		_, err := GenerateTrace(p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("appears twice"))
	})
})
