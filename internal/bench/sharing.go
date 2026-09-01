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

import "fmt"

// Sharing-mode sizing, in code rather than in a page, because the page cannot refuse.
//
// M5-c puts more than one engine on one card. Time-slicing and MPS do NOT partition memory -- the
// processes share one pool, so each engine's gpu-memory-utilization is a slice of the same total and the
// slices have to add up. Get that wrong and nothing warns you: the second engine starts, profiles, finds
// almost no room for a KV cache, and serves a handful of tokens per sequence. It looks like a working
// experiment and measures the allocator.
//
// The arithmetic below decided the card. Two Qwen2.5-3B engines on a T4 leave 10 MiB of KV each -- 284
// tokens, less than one prompt -- so the sharing matrix cannot run on the card M5-b uses. On an A10G it
// leaves 3.6 GiB each. That is a hardware conclusion reached before renting the hardware.

// GPUModel is a served model's memory shape, in the units a sizing decision needs.
type GPUModel struct {
	// Name is the served model identifier.
	Name string
	// WeightsMiB is the on-card cost of the parameters at the serving dtype.
	WeightsMiB int
	// KVBytesPerToken is 2 (K and V) * layers * kv_heads * head_dim * dtype_bytes.
	KVBytesPerToken int
}

// GPUCard is a card's usable memory as the driver reports it.
type GPUCard struct {
	// Name identifies the card.
	Name string
	// TotalMiB is what nvidia-smi reports, not the marketing capacity.
	//
	// Vendor-reported and NOT measured here. Confirm it on the card in the session's first minute; every
	// figure derived from it inherits the error, and a card that reports less than assumed is the case that
	// silently produces a useless run rather than a failed one.
	TotalMiB int
}

// SharingPlan is one card hosting some number of engines, and what that leaves for KV cache.
type SharingPlan struct {
	Card GPUCard
	// Model is the model every engine on the card serves; the matrix compares sharing modes, so varying the
	// model between engines would vary two things at once.
	Model GPUModel
	// Engines is how many vLLM processes share the card. One is the exclusive arm.
	Engines int
	// UtilizationPerEngine is each engine's --gpu-memory-utilization.
	//
	// Engines * UtilizationPerEngine must not exceed 1: the processes draw on one pool, and a device plugin
	// configured for time-slicing or MPS advertises more devices without creating more memory.
	UtilizationPerEngine float64
	// NonKVOverheadMiB is activations, CUDA graphs and allocator slack per engine.
	//
	// ESTIMATED, and the only estimated input here. vLLM prints the block count it actually allocated;
	// compare KVTokensPerEngine against it at session start rather than trusting this.
	NonKVOverheadMiB int
}

// BudgetPerEngineMiB is the memory one engine may address.
func (p SharingPlan) BudgetPerEngineMiB() float64 {
	if p.Engines <= 0 {
		return 0
	}
	return float64(p.Card.TotalMiB) * p.UtilizationPerEngine
}

// KVMiBPerEngine is what remains for the KV cache after weights and overhead.
func (p SharingPlan) KVMiBPerEngine() float64 {
	return p.BudgetPerEngineMiB() - float64(p.Model.WeightsMiB) - float64(p.NonKVOverheadMiB)
}

// KVTokensPerEngine is the predicted KV capacity, which the engine's own startup log can contradict.
func (p SharingPlan) KVTokensPerEngine() int {
	kv := p.KVMiBPerEngine()
	if kv <= 0 || p.Model.KVBytesPerToken <= 0 {
		return 0
	}
	return int(kv * 1024 * 1024 / float64(p.Model.KVBytesPerToken))
}

// minUsefulKVTokens is the floor below which a plan is not a smaller experiment but a different one.
//
// An engine whose cache cannot hold a few of the trace's own prompts spends its life evicting, so what the
// arm measures is recomputation rather than the sharing mode under test. The contender prompt measures
// 7,695 tokens (internal/bench/testdata/tokenizer_calibration.json); four of them concurrently is the
// smallest cache that lets the scheduler batch at all.
const minUsefulKVTokens = 4 * 7695

// Validate refuses a plan that cannot produce a result, and says which way it fails.
//
// It is deliberately not a warning. A plan that overcommits the card is not a degraded experiment, it is an
// experiment whose numbers describe the allocator, and the place to find that out is here rather than after
// the card is rented.
func (p SharingPlan) Validate() error {
	if p.Engines <= 0 {
		return fmt.Errorf("a plan needs at least one engine, got %d", p.Engines)
	}
	if p.UtilizationPerEngine <= 0 || p.UtilizationPerEngine > 1 {
		return fmt.Errorf("gpu-memory-utilization per engine must be in (0,1], got %v", p.UtilizationPerEngine)
	}
	if total := float64(p.Engines) * p.UtilizationPerEngine; total > 1.0 {
		return fmt.Errorf("%d engines at %.2f utilization claim %.2f of one card: time-slicing and MPS share one memory pool, they do not create a second",
			p.Engines, p.UtilizationPerEngine, total)
	}
	if kv := p.KVMiBPerEngine(); kv <= 0 {
		return fmt.Errorf("%s on %s with %d engines leaves %.0f MiB for KV cache after %d MiB of weights and %d MiB of overhead: the engine will not start",
			p.Model.Name, p.Card.Name, p.Engines, kv, p.Model.WeightsMiB, p.NonKVOverheadMiB)
	}
	if tok := p.KVTokensPerEngine(); tok < minUsefulKVTokens {
		return fmt.Errorf("%s on %s with %d engines leaves %d KV tokens per engine, below the %d needed to hold four contender prompts: the arm would measure eviction rather than the sharing mode",
			p.Model.Name, p.Card.Name, p.Engines, tok, minUsefulKVTokens)
	}
	return nil
}

// The cards and models the matrix was sized against. Figures are vendor-reported or computed from the
// model config, never guessed; see hack/m5c-sharing-sizing.md for where each came from.
var (
	// CardT4 is what M5-b runs on. It cannot host the sharing matrix, which is why M5-c does not use it.
	CardT4 = GPUCard{Name: "NVIDIA T4 16GB", TotalMiB: 15_360}
	// CardA10G is g5.xlarge: four vCPU, so it fits the same granted quota as g4dn.xlarge.
	CardA10G = GPUCard{Name: "NVIDIA A10G 24GB", TotalMiB: 23_028}

	// ModelQwen3B is M5-b's served model: 36 layers, 2 KV heads, head_dim 128, fp16.
	ModelQwen3B = GPUModel{Name: "Qwen/Qwen2.5-3B-Instruct", WeightsMiB: 5_886, KVBytesPerToken: 2 * 36 * 2 * 128 * 2}
)
