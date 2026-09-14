package bench

import (
	"fmt"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the study registry", func() {
	Describe("the price-of-protection sweep's arms", func() {
		It("names exactly the ten arms the pre-registration designs", func() {
			// Written out rather than regenerated from the same factors the code uses.
			//
			// A test that builds the expected names with the production function proves the function is
			// consistent with itself and nothing else. These strings are what the runner passes on the
			// command line and what a reader sees in the report, so they are pinned literally: changing
			// one has to be a deliberate edit here, not a side effect of touching a format string.
			s, ok := LookupStudy(StudyPriceOfProtection)
			Expect(ok).To(BeTrue())
			Expect(s.Arms).To(Equal([]string{
				"R1",
				"default-fcfs",
				"mbt-0256-fcfs",
				"mbt-0256-priority",
				"mbt-0512-fcfs",
				"mbt-0512-priority",
				"mbt-1024-fcfs",
				"mbt-1024-priority",
				"mbt-2048-fcfs",
				"mbt-2048-priority",
			}))
		})

		It("orders the cells numerically when sorted as strings", func() {
			// The zero padding is the whole reason the names are not b256/b512/b1024.
			//
			// Without it a plain sort gives 1024, 2048, 256, 512 -- a table whose budget column counts
			// down and then up, which a reader corrects for silently and a script does not correct for
			// at all.
			var cells []string
			for _, budget := range priceOfProtectionBudgets {
				cells = append(cells, PriceOfProtectionArm(budget, "fcfs"))
			}
			sorted := append([]string(nil), cells...)
			sort.Strings(sorted)
			Expect(sorted).To(Equal(cells), "lexical order must be numeric order")
		})

		It("refuses to admit an arm from the other study", func() {
			pop, _ := LookupStudy(StudyPriceOfProtection)
			m5b, _ := LookupStudy(StudyM5BGateway)

			// The specific collision the study field exists to prevent. The pre-registration's readings
			// call this study's control "off", and M5-b has an arm by that name which ran through a
			// gateway. Neither study may admit the other's condition.
			Expect(pop.Admits("off")).To(BeFalse())
			Expect(pop.Admits("kv-aware")).To(BeFalse())
			Expect(m5b.Admits("default-fcfs")).To(BeFalse())
			Expect(m5b.Admits("mbt-0512-priority")).To(BeFalse())

			// R1 is the one name both studies legitimately share: an isolated premium baseline means the
			// same thing in each, and both measure it as their ceiling.
			Expect(pop.Admits(ArmR1)).To(BeTrue())
			Expect(m5b.Admits(ArmR1)).To(BeTrue())
		})
	})

	Describe("evidence that predates the study field", func() {
		It("reads an empty study as the gateway experiment", func() {
			// Every raw file this repository has written so far carries no study at all. They are M5-b's,
			// because M5-b is the only experiment that has been run, and a report has to keep reading
			// them.
			s, ok := LookupStudy("")
			Expect(ok).To(BeTrue())
			Expect(s.ID).To(Equal(StudyM5BGateway))
		})

		It("does not invent a study for an identifier nobody registered", func() {
			_, ok := LookupStudy("price-of-protection")
			Expect(ok).To(BeFalse(), "a near-miss on a real study ID must not resolve")
		})
	})

	Describe("the report's arm column", func() {
		It("is wide enough for the longest arm any study defines", func() {
			// This was the literal 12, sized for M5-b's four names. "mbt-0512-priority" is 17, so every
			// column to its right would have been pushed out of line in the one table a reader looks at
			// -- and nothing would have failed. Deriving the width means a new study cannot do that.
			widest := 0
			var widestName string
			for _, id := range KnownStudyIDs() {
				s, _ := LookupStudy(id)
				for _, a := range s.Arms {
					if len(a) > widest {
						widest, widestName = len(a), a
					}
				}
			}
			Expect(ArmColumnWidth).To(BeNumerically(">=", widest),
				fmt.Sprintf("arm %q does not fit", widestName))
		})

		It("keeps the columns aligned for the longest name", func() {
			// The property that matters is alignment, so it is checked by formatting rather than by
			// asserting a number equal to the one the production code computed.
			short := fmt.Sprintf("%-*s|", ArmColumnWidth, ArmR1)
			long := fmt.Sprintf("%-*s|", ArmColumnWidth, "mbt-0512-priority")
			Expect(short).To(HaveLen(len(long)))
		})
	})
})
