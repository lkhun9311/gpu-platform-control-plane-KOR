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
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PromptText", func() {
	It("returns exactly the requested length, whether that cuts the corpus short or tiles it", func() {
		for _, n := range []int{1, 7, len(promptCorpus) - 1, len(promptCorpus), len(promptCorpus) + 1, 40000} {
			Expect(PromptText(n)).To(HaveLen(n), "length %d", n)
		}
		Expect(PromptText(0)).To(BeEmpty())
		Expect(PromptText(-5)).To(BeEmpty())
	})

	It("is a pure function of n, which is what lets separate arms replay one trace as identical bytes", func() {
		Expect(PromptText(40000)).To(Equal(PromptText(40000)))
	})

	It("stays ASCII, so cutting it to a byte length can never split a rune", func() {
		s := PromptText(len(promptCorpus) + 13)
		Expect(utf8.ValidString(s)).To(BeTrue())
		Expect(utf8.RuneCountInString(s)).To(Equal(len(s)))
	})

	It("does not send a run of one character, which a tokenizer would collapse", func() {
		// The prompt this replaces was strings.Repeat("x", n). Measured against the 40,000-character
		// contender prompt, Qwen2.5 read that as 5,029 tokens and Phi-3 as 10,004 -- so the frozen trace
		// loaded one served model twice as hard as the other while reporting the same traffic. The property
		// that broke is bounded vocabulary diversity, so that is what this asserts rather than the token
		// counts themselves, which no GPU-free test can recompute.
		s := PromptText(40000)
		distinct := map[byte]struct{}{}
		for i := range len(s) {
			distinct[s[i]] = struct{}{}
		}
		Expect(len(distinct)).To(BeNumerically(">", 20))
		Expect(len(strings.Fields(s))).To(BeNumerically(">", 5000))
	})
})

var _ = Describe("PromptCorpusSHA256", func() {
	It("matches the value the frozen manifests were written against", func() {
		// Pinned deliberately. Editing the corpus changes what every arm sends, so it must invalidate the
		// manifests rather than silently redefine the traffic they claim to have replayed; this assertion is
		// what turns such an edit into a failing build instead of an unexplained shift between runs.
		Expect(PromptCorpusSHA256).To(Equal("dec1020701584417449f9c7efa5354073682cce69348af3b9fb5244705200a86"))
	})
})
