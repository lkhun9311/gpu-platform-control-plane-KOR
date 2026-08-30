package queuelab

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// dcgmUtilisationMetric is the one series this reads.
//
// DCGM exports dozens; this takes GPU utilisation because it is the only one that answers the question the
// axis asks -- did the card do work -- rather than how much memory was reserved, which is allocation again
// under a different name.
const dcgmUtilisationMetric = "DCGM_FI_DEV_GPU_UTIL"

// errUnreadableValue marks one exposition line this build cannot read, as distinct from a scrape it cannot
// read. It is a sentinel rather than a string so the caller can tell the two apart without matching prose.
var errUnreadableValue = errors.New("utilisation value is not an integer percent")

// PodResolver maps a namespace and Pod NAME to the Pod's UID, or "" when the caller never observed it.
//
// The split is the honest part of this design. DCGM labels its samples with a Pod name because that is what
// the kubelet told it; the API is what says which object that name referred to. Letting the observer supply
// the UID as well would make it the authority on both what the card did AND whose card it was, and the second
// half is not its to assert.
//
// The caller builds this from its own watch rather than a live lookup, because by the time a run is
// reconstructed the Pod is gone and a live lookup would resolve nothing -- or worse, resolve a name that has
// since been reused.
type PodResolver func(namespace, name string) string

// ParseDCGMUtilisation turns one scrape of a DCGM exporter into samples, and reports how many it could not
// attribute.
//
// The unattributed count is returned rather than logged because it is evidence. A scrape naming Pods the
// collector never saw means the observer and the run disagree about what was on the node, and a caller that
// silently dropped those would be reporting a clean observation of a cluster it did not fully see. Samples
// with no Pod label at all are a different thing and are not counted: an idle card belongs to nobody, and
// DCGM emits it every scrape.
//
// Unattributable samples are also RETURNED rather than merely counted. They cannot credit work to anyone,
// and they are what the exclusivity clause needs: a device carrying two Pods' labels has utilisation that
// belongs to neither, and a parser that dropped the second label would hide that.
func ParseDCGMUtilisation(body []byte, atNs int64, resolve PodResolver) ([]DeviceSample, int, int, error) {
	if resolve == nil {
		return nil, 0, 0, fmt.Errorf("no Pod resolver: the observer labels by name and something else has to say " +
			"which object that name referred to")
	}
	var out []DeviceSample
	unattributed := 0
	unlabelledBusy := 0
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, dcgmUtilisationMetric) {
			continue
		}
		labels, value, err := splitMetricLine(line)
		switch {
		case errors.Is(err, errUnreadableValue):
			continue
		case err != nil:
			return nil, 0, 0, fmt.Errorf("scrape at %d ns: %w", atNs, err)
		}
		// A card with no Pod label is EITHER idle and unowned OR busy and unattributed, and those are not the
		// same fact. The second is what a kubelet mapping that is off or broken produces: full utilisation
		// with nothing naming a tenant. Skipping both silently sent an operator after the driver, because
		// the only message left was that the observer produced no sample for the Pod -- which reads as an
		// absent card rather than as absent attribution.
		//
		// Idle-and-unowned is still skipped without comment; a card nobody is using is not evidence about
		// anything. Busy-and-unlabelled is counted, so a caller can say which of the two it is looking at.
		if labels["pod"] == "" {
			if value > 0 {
				unlabelledBusy++
			}
			continue
		}
		if labels["UUID"] == "" {
			return nil, 0, 0, fmt.Errorf("scrape at %d ns: a sample for pod %s/%s names no device UUID",
				atNs, labels["namespace"], labels["pod"])
		}
		uid := resolve(labels["namespace"], labels["pod"])
		if uid == "" {
			unattributed++
		}
		// KEPT even when it cannot be attributed, which is a change from dropping it and the reason is the
		// exclusivity clause in EstablishesDeviceWork. A sample naming a Pod this run never saw is useless for
		// crediting work and decisive for refusing it: if that Pod is on the same device, the device was not
		// exclusively the victim's, and a parser that discarded the row would leave the verdict blind to
		// precisely the case that makes a utilisation reading unattributable.
		out = append(out, DeviceSample{
			AtNs:               atNs,
			DeviceUUID:         labels["UUID"],
			PodRef:             labels["namespace"] + "/" + labels["pod"],
			PodUID:             uid,
			UtilisationPercent: value,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("read scrape at %d ns: %w", atNs, err)
	}
	return out, unattributed, unlabelledBusy, nil
}

// splitMetricLine pulls the labels and the value out of one Prometheus exposition line.
//
// It is hand-written rather than pulled from a parsing library because the whole surface is one metric with
// quoted label values and an integer, and because a dependency that silently accepts a malformed line would
// turn a broken observer into a quiet one. Anything it cannot read is an error rather than a skip.
func splitMetricLine(line string) (map[string]string, int, error) {
	open := strings.IndexByte(line, '{')
	closeAt := strings.LastIndexByte(line, '}')
	if open < 0 || closeAt < open {
		return nil, 0, fmt.Errorf("no label set in %q", line)
	}
	labels := map[string]string{}
	for _, pair := range splitLabels(line[open+1 : closeAt]) {
		before, after, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, 0, fmt.Errorf("label %q in %q has no value", pair, line)
		}
		labels[strings.TrimSpace(before)] = strings.Trim(strings.TrimSpace(after), `"`)
	}
	raw := strings.TrimSpace(line[closeAt+1:])
	// Utilisation is an integer percent. A float would parse and then round somewhere invisible, so it is
	// refused here where the reason can be given.
	if raw == "" {
		// STRUCTURAL, not transient: a line with a label set and nothing after it is not a sample whose value
		// this build cannot read, it is not a sample. Skipping it would let an exposition format that stopped
		// carrying values at all be read as a card nobody was using.
		return nil, 0, fmt.Errorf("no value in %q", line)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		// An UNREADABLE ROW rather than a broken scrape, and the difference decides whether one bad line
		// ends the run's observation. DCGM renders unsupported or transiently unavailable field values as
		// the string "N/A" -- around driver initialisation, a GPU reset, an XID event. Treating that as a
		// fatal parse error ends the scrape loop, truncates the observation at that instant, and the run
		// then fails COVERAGE twenty seconds later with a message about an interval that was never watched.
		// The actual cause reaches nobody: it is one stderr line, and it is not in the record.
		//
		// So it is skipped and counted, exactly as an unattributable sample is, and the caller decides what
		// a scrape full of them means.
		return nil, 0, errUnreadableValue
	}
	if v < 0 || v > 100 {
		return nil, 0, fmt.Errorf("utilisation %d in %q is outside 0-100", v, line)
	}
	return labels, v, nil
}

// splitLabels splits a label set on commas that are not inside a quoted value.
//
// DCGM's modelName carries commas on some cards ("NVIDIA A100-SXM4-40GB, MIG 1g.5gb"), and a naive split
// would produce a fragment with no '=' and turn a perfectly good scrape into a parse error.
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
