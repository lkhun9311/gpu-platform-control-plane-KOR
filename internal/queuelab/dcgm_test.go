package queuelab

import (
	"strings"
	"testing"
)

// renderedScrape is the shape a DCGM exporter actually emits, including the series this ignores, an idle card
// with no Pod labels, and a model name containing a comma.
// renderedScrape follows the template the PINNED exporter binary carries, read out of
// nvcr.io/nvidia/k8s/dcgm-exporter@sha256:3d4e0dfa... rather than out of the documentation:
//
//	{{ $counter.FieldName }}{gpu="…",{{ $metric.UUID }}="…",pci_bus_id="…",device="…",modelName="…"
//	  {{if $metric.MigProfile}},GPU_I_PROFILE="…",GPU_I_ID="…"{{end}}
//	  {{if $metric.Hostname}},Hostname="…"{{end}}
//	  {{- range $k, $v := $metric.Attributes -}},{{ $k }}="{{ $v }}"{{- end -}}} value
//
// It is RENDERED, not captured: nv-hostengine in that image exposes no fake-GPU mode, so no exposition
// could be produced without a card. That is better than documentation and weaker than a scrape, and the
// name says so -- it used to be called renderedScrape, which it never was.
//
// pci_bus_id was missing until this audit. Harmless, because labels split on commas outside quotes and the
// key ends at the first '=' -- but a fixture that does not match the shape it stands in for cannot be
// evidence about that shape.
const renderedScrape = `# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-aaaa",pci_bus_id="00000000:00:1E.0",device="nvidia0",modelName="NVIDIA A10G",Hostname="w1",container="trainer",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 97
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-bbbb",pci_bus_id="00000000:00:1F.0",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB, MIG 1g.5gb",Hostname="w1",container="trainer",namespace="queuelab-r1",pod="b1-owner-m4t9q"} 3
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-cccc",pci_bus_id="00000000:00:20.0",device="nvidia2",modelName="NVIDIA A10G",Hostname="w1"} 0
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-aaaa",Hostname="w1",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 8192
`

// resolver maps the two Pods the collector would have observed.
func resolver(ns, name string) string {
	switch ns + "/" + name {
	case "queuelab-r1/a2-borrow-x7k2p":
		return "victim-uid"
	case "queuelab-r1/b1-owner-m4t9q":
		return "owner-uid"
	}
	return ""
}

// The ordinary scrape: two attributed samples, the idle card skipped, the other metric ignored, and the
// comma inside a model name not breaking the label split.
func TestParseDCGMTakesUtilisationAndAttributesIt(t *testing.T) {
	got, unattributed, _, err := ParseDCGMUtilisation([]byte(renderedScrape), 5_000_000_000, resolver)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if unattributed != 0 {
		t.Fatalf("unattributed = %d, want 0: every labelled Pod is one the resolver knows", unattributed)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2 (the idle card has no Pod and DCGM_FI_DEV_FB_USED is not utilisation): %+v",
			len(got), got)
	}
	// Both carry the name the exporter used as well as the identity the API supplied, because the exclusivity
	// clause reads names and the attribution reads identities.
	for _, smp := range got {
		if smp.PodRef == "" {
			t.Fatalf("a sample carries no PodRef: %+v", smp)
		}
	}
	byUID := map[string]DeviceSample{}
	for _, s := range got {
		byUID[s.PodUID] = s
	}
	if v := byUID["victim-uid"]; v.DeviceUUID != "GPU-aaaa" || v.UtilisationPercent != 97 || v.AtNs != 5_000_000_000 {
		t.Fatalf("victim sample = %+v", v)
	}
	// The card whose model name contains a comma must still parse; a naive split on commas produces a
	// fragment with no '=' and turns a good scrape into an error.
	if o := byUID["owner-uid"]; o.DeviceUUID != "GPU-bbbb" || o.UtilisationPercent != 3 {
		t.Fatalf("owner sample = %+v; the model name's comma broke the label split", o)
	}
}

// A card nobody is using carries no Pod labels every scrape. That is not an attribution failure and must not
// be counted as one, or every observation would look partial.
func TestAnIdleCardIsNotAnAttributionFailure(t *testing.T) {
	_, unattributed, _, err := ParseDCGMUtilisation(
		[]byte(`DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-cccc",Hostname="w1"} 0`+"\n"), 1, resolver)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if unattributed != 0 {
		t.Fatalf("an idle card counted as unattributed (%d); an observation would read as partial every scrape",
			unattributed)
	}
}

// A Pod the collector never saw is evidence, not noise: the observer and the run disagree about what was on
// the node, and dropping it silently would report a clean observation of a cluster nobody fully saw.
func TestAPodTheCollectorNeverSawIsCounted(t *testing.T) {
	line := `DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-dddd",namespace="other",pod="stranger-9xk"} 55` + "\n"
	got, unattributed, _, err := ParseDCGMUtilisation([]byte(line), 1, resolver)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if unattributed != 1 {
		t.Fatalf("unattributed = %d, want 1", unattributed)
	}
	// It is KEPT rather than dropped, carrying no UID and the name the exporter used. It can credit work to
	// nobody, and it is what the exclusivity clause needs: a device carrying two Pods' labels has utilisation
	// belonging to neither, and a parser that discarded the second label would hide exactly that.
	if len(got) != 1 {
		t.Fatalf("an unresolvable sample was dropped; the exclusivity check would be blind to it: %+v", got)
	}
	if got[0].PodUID != "" {
		t.Fatalf("an unresolvable sample was given the UID %q", got[0].PodUID)
	}
	if got[0].PodRef != "other/stranger-9xk" {
		t.Fatalf("PodRef = %q, want the name the exporter used", got[0].PodRef)
	}
}

// A sample naming a Pod but no card cannot say which device did anything, and that is an error rather than a
// skip: silently dropping it would hide a broken exporter behind a thinner observation.
func TestASampleWithoutADeviceUUIDIsAnError(t *testing.T) {
	line := `DCGM_FI_DEV_GPU_UTIL{gpu="0",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 40` + "\n"
	if _, _, _, err := ParseDCGMUtilisation([]byte(line), 1, resolver); err == nil {
		t.Fatal("a sample naming no device parsed")
	}
}

// Anything the parser cannot read is an error. A dependency that silently accepted malformed lines would
// turn a broken observer into a quiet one.
func TestMalformedScrapesAreRefusedRatherThanSkipped(t *testing.T) {
	for name, line := range map[string]string{
		"no label set":    `DCGM_FI_DEV_GPU_UTIL 97`,
		"out of range":    `DCGM_FI_DEV_GPU_UTIL{UUID="G",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 4000`,
		"label with no =": `DCGM_FI_DEV_GPU_UTIL{UUID="G",broken,namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 10`,
		"no value at all": `DCGM_FI_DEV_GPU_UTIL{UUID="G",namespace="queuelab-r1",pod="a2-borrow-x7k2p"}`,
	} {
		if _, _, _, err := ParseDCGMUtilisation([]byte(line+"\n"), 1, resolver); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// The observer labels by name; something else has to say which object that name referred to. Without a
// resolver there is nothing to attribute to, and inventing an identity is the one thing this must not do.
func TestParsingWithoutAResolverIsRefused(t *testing.T) {
	_, _, _, err := ParseDCGMUtilisation([]byte(renderedScrape), 1, nil)
	if err == nil {
		t.Fatal("a scrape parsed with nothing to resolve Pod identity")
	}
	if !strings.Contains(err.Error(), "which object that name referred to") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}

// A value this build cannot read is one unreadable ROW, not a broken scrape, and that distinction decides
// whether one bad line ends a run's observation.
//
// DCGM renders unsupported or transiently unavailable field values as "N/A" -- around driver initialisation,
// a GPU reset, an XID event. Treating that as a fatal parse error ends the scrape loop, truncates the
// observation at that instant, and the run then fails COVERAGE twenty seconds later with a message about an
// interval that was never watched. The actual cause reaches nobody: it is one line on stderr and it is not
// in the record.
//
// A value that is ABSENT is a different thing and stays fatal. A line carrying a label set and nothing after
// it is not a sample whose value went unavailable; it is not a sample, and skipping it would let an
// exposition format that stopped carrying values be read as a card nobody was using.
//
// Mutation that turns this red: make the non-integer case fatal again, or make the empty case skip.
func TestATransientlyUnreadableValueSkipsItsRowRatherThanTheScrape(t *testing.T) {
	resolve := func(ns, name string) string { return "uid-1" }
	body := []byte(`DCGM_FI_DEV_GPU_UTIL{UUID="G0",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} N/A
DCGM_FI_DEV_GPU_UTIL{UUID="G0",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 91
DCGM_FI_DEV_GPU_UTIL{UUID="G1",namespace="queuelab-r1",pod="a2-borrow-x7k2p"} 97.5
`)
	samples, _, _, err := ParseDCGMUtilisation(body, 1_000, resolve)
	if err != nil {
		t.Fatalf("one unreadable value ended the whole scrape, which truncates the observation and surfaces "+
			"twenty seconds later as a coverage failure about an interval nobody watched: %v", err)
	}
	if len(samples) != 1 || samples[0].UtilisationPercent != 91 {
		t.Fatalf("the readable rows did not survive alongside the unreadable ones: %+v", samples)
	}

	// Absent is still fatal.
	if _, _, _, err := ParseDCGMUtilisation(
		[]byte(`DCGM_FI_DEV_GPU_UTIL{UUID="G0",namespace="queuelab-r1",pod="a2-borrow-x7k2p"}`+"\n"),
		1_000, resolve); err == nil {
		t.Fatal("a line carrying no value at all parsed, so an exposition that stopped carrying values would " +
			"read as a card nobody was using")
	}
}

// A busy card with no Pod label is attribution failing while the hardware runs, and it must not read as an
// idle card.
//
// This is what an exporter whose kubernetes mapping is off produces -- and that mapping is OFF BY DEFAULT.
// Both cases used to be skipped in silence, so the only message an operator saw was that the observer
// produced no sample for the Pod, which sends them to the workload when the fault is in the exporter.
//
// Mutation that turns this red: drop the counter, or count idle rows too.
func TestABusyCardWithNoPodLabelIsCountedRatherThanTreatedAsIdle(t *testing.T) {
	resolve := func(ns, name string) string { return "uid-1" }
	body := []byte(`DCGM_FI_DEV_GPU_UTIL{UUID="G0",device="nvidia0"} 94
DCGM_FI_DEV_GPU_UTIL{UUID="G1",device="nvidia1"} 0
`)
	samples, _, unlabelledBusy, err := ParseDCGMUtilisation(body, 1_000, resolve)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("a row naming no Pod was attributed to one: %+v", samples)
	}
	if unlabelledBusy != 1 {
		t.Fatalf("unlabelledBusy = %d, want 1: the idle card must not be counted and the busy one must, or "+
			"the two failures this counter separates collapse back together", unlabelledBusy)
	}
}
