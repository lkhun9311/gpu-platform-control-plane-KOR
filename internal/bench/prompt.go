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

import "strings"

// promptCorpus is the text every generated prompt is cut from.
//
// It replaces strings.Repeat("x", n), which was chosen for convenience and quietly decided how hard the
// experiment pushes the engine. A byte-pair tokenizer merges long runs of one character into single
// tokens, and how far it merges is a property of the vocabulary rather than of the traffic: measured
// against the 40,000-character contender prompt, Qwen2.5 turned it into 5,029 tokens and Phi-3 into
// 10,004. The same frozen trace therefore placed twice the KV-cache load on one served model as on the
// other, while the manifest reported both runs as the same traffic. Ordinary prose does not behave that
// way -- the same two tokenizers read 40,000 characters of it as 9,299 and 11,511 tokens, a spread of
// vocabulary size rather than of payload shape.
//
// The subject matter is this project's own, so the text sits in the register a serving gateway actually
// receives rather than in a literary one, and it is ASCII so that cutting it to a byte length can never
// split a rune.
const promptCorpus = `A scheduler that admits work it cannot finish has not scheduled anything; it has only ` +
	`deferred the moment someone notices. The queue is where that deferral becomes visible, and the depth of ` +
	`the queue is the only honest statement a serving system makes about its own capacity. When the cache ` +
	`that holds attention state fills, the engine does not slow down gracefully. It begins to evict, and an ` +
	`eviction is not a small cost paid once but a recomputation paid again on every token that follows. The ` +
	`admission decision is therefore the last cheap decision in the request's life: everything after it is ` +
	`priced in accelerator time. A guard placed there has to answer from the engine's own telemetry rather ` +
	`than from a fixed rate, because a fixed rate encodes an assumption about prompt length that the traffic ` +
	`is free to violate. Two tenants sharing one pool of memory do not share it symmetrically. The tenant ` +
	`sending long contexts consumes cache in proportion to what it sends, while the tenant sending short ones ` +
	`consumes it in proportion to what it receives, and the second tenant pays for the first through a ` +
	`latency it has no way to attribute. Measuring that transfer requires holding the arrival schedule fixed ` +
	`while changing only the policy, which is harder than it sounds, because most of the knobs that look like ` +
	`policy are really capacity in disguise. Rejecting more requests will improve the latency of the ones ` +
	`that remain under any policy whatsoever, so a comparison that does not match admitted load is measuring ` +
	`its own thresholds. The control arm exists to make that confound expensive to ignore rather than easy to ` +
	`forget, and it earns its place only if it is tuned to admit the same volume as the arm under test.
`

// PromptCorpusSHA256 identifies the corpus the running binary will send.
//
// The trace checksum cannot cover this. A trace row records a prompt LENGTH, and the text of that length is
// synthesised here at send time, so two binaries built from different corpora replay the same checksummed
// trace as different bytes on the wire while every arm reports the same traceChecksum. The manifest carries
// this value so the mismatch is refused at load instead of surviving into the report as an unexplained
// difference between arms.
var PromptCorpusSHA256 = Checksum([]byte(promptCorpus))

// PromptText returns exactly n characters of prompt text, deterministically.
//
// Callers ask for a length because the trace records a length; tiling one corpus to reach it keeps the
// result a pure function of n, so the same trace row produces identical bytes in every arm and on every
// replay. n <= 0 yields the empty string rather than an error, matching what a zero-length row means.
func PromptText(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n)
	for b.Len() < n {
		if remaining := n - b.Len(); remaining < len(promptCorpus) {
			b.WriteString(promptCorpus[:remaining])
			break
		}
		b.WriteString(promptCorpus)
	}
	return b.String()
}
