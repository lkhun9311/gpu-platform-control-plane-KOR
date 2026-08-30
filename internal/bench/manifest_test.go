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
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/yaml"
)

// writeTestManifest writes a minimal valid manifest YAML plus a matching trace file into dir,
// returning the manifest path.
//
// Both files live in the same directory so the manifest's relative tracePath resolves without a
// caller having to know the layout.
func writeTestManifest(dir string, mutate func(m *RunManifest)) string {
	traceContent := []byte(`{"index":0,"offsetMs":0,"tenant":"premium-1","promptLenChars":100,"maxOutputTokens":64,"isNoisy":false}` + "\n")
	tracePath := filepath.Join(dir, "trace.jsonl")
	Expect(os.WriteFile(tracePath, traceContent, 0o600)).To(Succeed())
	sum := Checksum(traceContent)

	m := RunManifest{
		SchemaVersion:   "v2",
		PromptCorpusSHA: PromptCorpusSHA256,
		Arm:             "R1",
		GatewayURL:      "http://gateway.example:8080",
		TracePath:       "trace.jsonl",
		TraceChecksum:   sum,
		Model:           "llama-3-8b",
		TimeoutMs:       30000,
		Seed:            42,
		PrimaryEndpoint: "ttft_p99",
		MatchTolerance:  "0.05",
	}
	if mutate != nil {
		mutate(&m)
	}

	data, err := yaml.Marshal(&m)
	Expect(err).NotTo(HaveOccurred())
	manifestPath := filepath.Join(dir, "manifest.yaml")
	Expect(os.WriteFile(manifestPath, data, 0o600)).To(Succeed())
	return manifestPath
}

var _ = Describe("LoadManifest", func() {
	It("loads a valid manifest and resolves its trace path to an absolute path", func() {
		dir := GinkgoT().TempDir()
		path := writeTestManifest(dir, nil)

		m, err := LoadManifest(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Arm).To(Equal("R1"))
		Expect(m.Model).To(Equal("llama-3-8b"))
		Expect(filepath.IsAbs(m.TracePath)).To(BeTrue())
	})

	It("hard-errors when the trace checksum does not match the trace file", func() {
		dir := GinkgoT().TempDir()
		path := writeTestManifest(dir, func(m *RunManifest) {
			m.TraceChecksum = "0000000000000000000000000000000000000000000000000000000000000000"
		})

		_, err := LoadManifest(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("checksum"))
	})

	It("refuses a manifest frozen against prompt bytes this binary does not send", func() {
		// traceChecksum cannot catch this. The trace records prompt LENGTHS and the text is synthesised at
		// send time, so a binary built from a different corpus replays the same checksummed trace as
		// different bytes on the wire -- and a report comparing that arm against another would attribute
		// the difference to the policy under test. Here the trace file is untouched and its checksum still
		// matches; only the corpus differs, and that alone must be enough to refuse the load.
		dir := GinkgoT().TempDir()
		path := writeTestManifest(dir, func(m *RunManifest) {
			m.PromptCorpusSHA = "1111111111111111111111111111111111111111111111111111111111111111"
		})

		_, err := LoadManifest(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("promptCorpusSHA mismatch"))
	})

	It("errors when the trace file the manifest points at does not exist", func() {
		dir := GinkgoT().TempDir()
		path := writeTestManifest(dir, func(m *RunManifest) {
			m.TracePath = "missing-trace.jsonl"
		})

		_, err := LoadManifest(path)
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("errors when a required field is missing",
		func(mutate func(m *RunManifest)) {
			dir := GinkgoT().TempDir()
			path := writeTestManifest(dir, mutate)

			_, err := LoadManifest(path)
			Expect(err).To(HaveOccurred())
		},
		Entry("empty schema version", func(m *RunManifest) { m.SchemaVersion = "" }),
		Entry("empty arm", func(m *RunManifest) { m.Arm = "" }),
		Entry("empty gateway URL", func(m *RunManifest) { m.GatewayURL = "" }),
		Entry("empty trace path", func(m *RunManifest) { m.TracePath = "" }),
		Entry("empty trace checksum", func(m *RunManifest) { m.TraceChecksum = "" }),
		Entry("empty model", func(m *RunManifest) { m.Model = "" }),
		Entry("non-positive timeout", func(m *RunManifest) { m.TimeoutMs = 0 }),
		Entry("negative timeout", func(m *RunManifest) { m.TimeoutMs = -1 }),
		Entry("empty primary endpoint", func(m *RunManifest) { m.PrimaryEndpoint = "" }),
		Entry("empty match tolerance", func(m *RunManifest) { m.MatchTolerance = "" }),
		Entry("unparsable match tolerance", func(m *RunManifest) { m.MatchTolerance = "not-a-number" }),
	)

	It("rejects an arm not in the allowed set", func() {
		dir := GinkgoT().TempDir()
		path := writeTestManifest(dir, func(m *RunManifest) { m.Arm = "arm-Z" })

		_, err := LoadManifest(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("arm"))
	})

	DescribeTable("accepts every allowed arm",
		func(arm string) {
			dir := GinkgoT().TempDir()
			path := writeTestManifest(dir, func(m *RunManifest) { m.Arm = arm })

			_, err := LoadManifest(path)
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("R1", "R1"),
		Entry("off", "off"),
		Entry("static-cap", "static-cap"),
		Entry("kv-aware", "kv-aware"),
	)

	It("errors on a nonexistent manifest file", func() {
		_, err := LoadManifest(filepath.Join(GinkgoT().TempDir(), "nope.yaml"))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("RequireProvenance", func() {
	// Two fields were declared on RunManifest for provenance and never set by anything: no gen-trace flag
	// accepted them, no script passed them, and every manifest this repository produced left them empty. A
	// paid run's numbers belong to a build, and once the cluster is gone the record is the only place that
	// association can live.

	full := func() RunManifest {
		return RunManifest{
			GatewaySHA: "0f3c1a9",
			ImageDigests: map[string]string{
				"gateway": "123.dkr.ecr.ap-northeast-2.amazonaws.com/gateway@sha256:" + strings.Repeat("a", 64),
				"engine":  "vllm/vllm-openai@sha256:" + strings.Repeat("b", 64),
			},
		}
	}

	It("accepts a manifest that names the build and digest-pins every image", func() {
		Expect(full().RequireProvenance()).To(Succeed())
	})

	It("refuses a manifest with no gateway SHA", func() {
		m := full()
		m.GatewaySHA = "   "
		Expect(m.RequireProvenance()).To(MatchError(ContainSubstring("no gatewaySHA")))
	})

	It("refuses a manifest missing a role the number depends on", func() {
		m := full()
		delete(m.ImageDigests, "engine")
		Expect(m.RequireProvenance()).To(MatchError(ContainSubstring(`names no image for "engine"`)))
	})

	It("refuses a tag, which is what the paid scripts actually used", func() {
		// hack/m5b-arms.sh defaulted GW_IMAGE to gateway:m5b. Recording that would be provenance theatre: the
		// name identifies whatever was pushed under it most recently, so it says nothing after the next build.
		m := full()
		m.ImageDigests["gateway"] = "gateway:m5b"
		err := m.RequireProvenance()
		Expect(err).To(MatchError(ContainSubstring("a tag rather than a digest")))
	})

	It("refuses a digest that is not a digest", func() {
		// A reference can contain @sha256: and still be nonsense. Truncated and non-hex both fail, so the
		// check is on the value rather than on the punctuation.
		for _, bad := range []string{
			"gateway@sha256:" + strings.Repeat("a", 63),
			"gateway@sha256:" + strings.Repeat("g", 64),
			"gateway@sha256:",
			"@sha256:" + strings.Repeat("a", 64),
		} {
			m := full()
			m.ImageDigests["gateway"] = bad
			Expect(m.RequireProvenance()).To(HaveOccurred(), "accepted %q", bad)
		}
	})
})
