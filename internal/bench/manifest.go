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

// Package bench implements the GPU-free benchmark harness: a frozen run manifest, an immutable
// open-loop trace, a replay client that records raw per-request evidence, and a report that
// computes the design's pre-registered checks from that evidence.
//
// Design rationale (design spec "Benchmarking harness" section): the gateway's own request
// histogram starts just before the proxy handoff and stops when the stream closes, so it is not
// TTFT. Every latency number this package reports comes from client-side raw timestamps recorded
// during replay, never from a server-side metric.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// RunManifest freezes everything that must stay fixed for a benchmark comparison across arms to
// be valid.
//
// Design rationale (design spec "Benchmarking harness" section): "Frozen run manifest (versioned,
// validated): arms, trace checksum, image digests, model/tokenizer revision, gateway SHA,
// thresholds, B rate/burst, endpoint, tolerances." Every arm's manifest for one repetition shares
// the same TracePath/TraceChecksum, Model, and Seed; only Arm and Thresholds are expected to
// differ between them.
type RunManifest struct {
	// SchemaVersion pins the manifest shape, so a future incompatible change to this struct can
	// be detected instead of silently misparsed.
	SchemaVersion string `json:"schemaVersion"`
	// Study names the pre-registered experiment this run belongs to.
	//
	// It exists because arm names are not unique across experiments and never were: M5-b's "off" is
	// the gateway with its guard disabled, and the price-of-protection run's control is the engine at
	// its default batch budget with no gateway at all. Without this field the two produce rows a
	// report cannot tell apart, and pooling them silently answers a question nobody asked.
	//
	// Empty means the evidence predates studies, and is read as StudyM5BGateway.
	Study string `json:"study,omitempty"`
	// Arm names which pre-registered condition of Study this run measures.
	//
	// The admissible values come from the study registry in study.go, not from a list here.
	Arm string `json:"arm"`
	// GatewayURL is the target the replay client sends requests to.
	GatewayURL string `json:"gatewayURL"`
	// TracePath locates the immutable trace file this run replays.
	//
	// A relative path is resolved against the manifest file's own directory by LoadManifest, so
	// a manifest and its trace can be moved together without editing this field.
	TracePath string `json:"tracePath"`
	// TraceChecksum is the sha256 (hex) of the trace file at TracePath, verified by LoadManifest.
	//
	// This is what makes "immutable trace across frozen arms" enforceable rather than aspirational:
	// a trace edited after the checksum was frozen fails to load instead of silently comparing arms
	// against different traffic.
	TraceChecksum string `json:"traceChecksum"`
	// Model is the model name every replayed request targets.
	Model string `json:"model"`
	// PromptCorpusSHA identifies the prompt text the run's binary sends, which traceChecksum cannot cover:
	// the trace records prompt LENGTHS and the text is synthesised at send time. LoadManifest refuses a
	// manifest whose corpus is not the one compiled into this binary, so a run frozen against different
	// prompt bytes cannot be replayed or reported as if it were the same traffic.
	PromptCorpusSHA string `json:"promptCorpusSHA"`

	// TokenizerRev records the tokenizer/chat-template revision the estimator was calibrated
	// against.
	//
	// Recorded for provenance only; the GPU-free build never loads a real tokenizer with it
	// (design spec "Frozen run manifest" bullet).
	TokenizerRev string `json:"tokenizerRev,omitempty"`
	// GatewaySHA is the gateway build's commit SHA, recorded for provenance.
	GatewaySHA string `json:"gatewaySHA,omitempty"`
	// ImageDigests pins every image (gateway, vLLM, load generator) this run used, by name.
	ImageDigests map[string]string `json:"imageDigests,omitempty"`
	// Thresholds records the guard/static-cap parameters in effect for this arm (e.g. engage
	// usage, W, static-cap rate/burst), as strings so heterogeneous parameter sets across arms
	// don't need one struct field per possible knob.
	Thresholds map[string]string `json:"thresholds,omitempty"`
	// TimeoutMs bounds how long the replay client waits for a single request before recording it
	// as a timeout row.
	TimeoutMs int `json:"timeoutMs"`
	// Seed is the trace generator's seed, recorded here so a manifest alone documents which seed
	// produced its trace even though gen-trace, not LoadManifest, is what actually consumes it.
	Seed int64 `json:"seed"`
	// PrimaryEndpoint names the pre-registered primary metric.
	//
	// Design spec "Pre-registered success criteria" section: "Primary endpoint is TTFT p99 ...
	// Choosing the endpoint now prevents post-hoc metric selection." Always "ttft_p99" in this
	// design, but recorded rather than hardcoded so the report can assert it was not changed
	// after the fact.
	PrimaryEndpoint string `json:"primaryEndpoint"`
	// MatchTolerance is the pre-registered admission-work match tolerance, as a decimal string
	// (e.g. "0.05" for +/-5%).
	//
	// A string, not a float64: the manifest is meant to be hand-edited YAML, and "0.05" round-trips
	// through YAML/JSON exactly, while a float field risks a value like 0.050000000000000003
	// appearing in a re-serialized manifest and looking like the tolerance was silently changed.
	MatchTolerance string `json:"matchTolerance"`
	// LongThreshold is the eligible-population token threshold the guard and static cap gate on.
	//
	// It is frozen here so the report scores admitted-work over the same population the guard used, even if the paid pilot tuned the gateway's --admission-long-threshold.
	LongThreshold int `json:"longThreshold,omitempty"`
}

// LoadManifest reads, validates, and returns the manifest at path.
//
// Validation covers two independent things, both hard errors: every required field is present
// and well-formed, and TraceChecksum matches the sha256 of the trace file TracePath resolves to.
// The second check is what makes an edited-after-freeze trace file unusable rather than silently
// accepted, which is the property the design spec calls "immutable trace across frozen arms".
//
// On success, m.TracePath is rewritten to the resolved absolute path, so callers (gen-trace,
// replay) never need to re-resolve it against the manifest's own directory.
func LoadManifest(path string) (*RunManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}

	var m RunManifest
	// sigs.k8s.io/yaml round-trips through JSON, so a manifest may be written as either YAML or
	// plain JSON (JSON is valid YAML) without this package needing to tell them apart.
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	if err := m.validateFields(); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}

	tracePath := m.TracePath
	if !filepath.IsAbs(tracePath) {
		tracePath = filepath.Join(filepath.Dir(path), tracePath)
	}
	sum, err := ChecksumFile(tracePath)
	if err != nil {
		return nil, fmt.Errorf("read trace file %s: %w", tracePath, err)
	}
	if sum != m.TraceChecksum {
		return nil, fmt.Errorf("trace checksum mismatch: manifest declares %s but %s hashes to %s",
			m.TraceChecksum, tracePath, sum)
	}
	m.TracePath = tracePath

	return &m, nil
}

// validateFields checks that every field LoadManifest's checksum step depends on, and every
// field the report later depends on, is present and well-formed.
//
// It does not touch the filesystem; that half of validation (the checksum check) lives in
// LoadManifest, since it needs the resolved trace path.
func (m *RunManifest) validateFields() error {
	required := []struct {
		name  string
		value string
	}{
		{"schemaVersion", m.SchemaVersion},
		{"arm", m.Arm},
		{"gatewayURL", m.GatewayURL},
		{"tracePath", m.TracePath},
		{"traceChecksum", m.TraceChecksum},
		{"model", m.Model},
		{"promptCorpusSHA", m.PromptCorpusSHA},
		{"primaryEndpoint", m.PrimaryEndpoint},
		{"matchTolerance", m.MatchTolerance},
	}
	for _, f := range required {
		if f.value == "" {
			return fmt.Errorf("missing required field %q", f.name)
		}
	}

	if m.PromptCorpusSHA != PromptCorpusSHA256 {
		return fmt.Errorf("promptCorpusSHA mismatch: manifest declares %s but this binary sends corpus %s",
			m.PromptCorpusSHA, PromptCorpusSHA256)
	}

	study, ok := LookupStudy(m.Study)
	if !ok {
		return fmt.Errorf("study %q is not registered; known studies are %s", m.Study, strings.Join(KnownStudyIDs(), ", "))
	}
	if !study.Admits(m.Arm) {
		return fmt.Errorf("arm %q is not one of study %s's arms (%s)", m.Arm, study.ID, strings.Join(study.Arms, ", "))
	}

	if m.TimeoutMs <= 0 {
		return fmt.Errorf("timeoutMs must be positive, got %d", m.TimeoutMs)
	}

	if _, err := strconv.ParseFloat(m.MatchTolerance, 64); err != nil {
		return fmt.Errorf("matchTolerance %q is not a number: %w", m.MatchTolerance, err)
	}

	return nil
}

// ProvenanceRoles are the images a paid run's number depends on, and therefore the ones its evidence has to
// name.
//
// The gateway is the component under test on arms C and B; the engine is what produces every latency the
// report quotes. A record that cannot say which build of either produced it is a number without a subject.
var ProvenanceRoles = []string{"gateway", "engine"}

// digestPinned reports whether a reference names an immutable image.
//
// A tag is not provenance. `gateway:m5b` identifies whatever was pushed under that name most recently, so a
// record carrying it says only that somebody ran something called m5b -- and the paid session scripts used
// exactly that form. Only `name@sha256:...` survives a rebuild.
func digestPinned(ref string) bool {
	i := strings.Index(ref, "@sha256:")
	if i <= 0 {
		return false
	}
	// 64 hex characters after the prefix, and nothing else.
	hex := ref[i+len("@sha256:"):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RequireProvenance refuses a manifest that cannot name the builds its numbers came from.
//
// GatewaySHA and ImageDigests were declared on RunManifest for exactly this and nothing ever set them: no
// gen-trace flag accepted them, no script passed them, and every manifest this repository has produced left
// them empty. The fields documented an intention.
//
// This is opt-in rather than always-on, in the manner of -require-device: a kind run against a stub has no
// build worth pinning, and making every free run carry one would push operators toward inventing a value.
// The paid path asks for it, because that is where the record has to outlive the cluster.
func (m RunManifest) RequireProvenance() error {
	if strings.TrimSpace(m.GatewaySHA) == "" {
		return fmt.Errorf("manifest carries no gatewaySHA; a paid run's evidence must name the build that produced it")
	}
	for _, role := range ProvenanceRoles {
		ref, ok := m.ImageDigests[role]
		if !ok || strings.TrimSpace(ref) == "" {
			return fmt.Errorf("manifest names no image for %q; imageDigests must pin every image the number depends on", role)
		}
		if !digestPinned(ref) {
			return fmt.Errorf("manifest pins %q to %q, which is a tag rather than a digest;"+
				" a tag names whatever was pushed under it most recently, so it identifies nothing after the next build", role, ref)
		}
	}
	return nil
}
