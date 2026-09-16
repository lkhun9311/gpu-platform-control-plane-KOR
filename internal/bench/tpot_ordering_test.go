package bench

import "testing"

// deliveredRow builds a completed premium row whose inter-token time is exactly tpotMs.
//
// TPOT is (end - firstToken) / (outputTokens - 1), so two tokens separated by tpotMs gives tpotMs.
func deliveredRow(index int, tpotMs float64) RawRow {
	const sec = int64(1_000_000_000)
	send := sec + int64(index)*sec
	first := send + 10_000_000
	return RawRow{
		Index: index, Arm: "off", Tenant: "premium-1", IsNoisy: false,
		SendUnixNanos: send, FirstTokenUnixNanos: first,
		EndUnixNanos: first + int64(tpotMs*1e6),
		OutputTokens: 2, EstInputTokens: 50, HTTPStatus: 200,
	}
}

// TestTPOTPercentilesAreTakenOverSortedSamples catches a wrong number, not a crash.
//
// percentile is documented as operating on a SORTED slice and every other caller sorts before calling it
// (report.go sorts ttft and e2e). The TPOT samples were not sorted, so TPOTMsP50 and TPOTMsP99 were
// whichever values happened to sit at those positions in arrival order.
//
// It matters because TPOT p99 is one of the five things the pre-registration requires every arm to
// report, and reading 1 fails a cell whose TPOT p99 exceeds 1.25x the isolated baseline. An unsorted p99
// is a number that looks like a tail and is not one, which is the failure this repository's measurement
// rule exists to prevent: a plausible wrong number is worse than a refusal.
func TestTPOTPercentilesAreTakenOverSortedSamples(t *testing.T) {
	// Deliberately out of order, and deliberately with the largest sample first so that an unsorted
	// nearest-rank lands nowhere near it.
	rows := []RawRow{
		deliveredRow(0, 100),
		deliveredRow(1, 1),
		deliveredRow(2, 2),
	}
	s := Summarize("off", rows)

	// Sorted, the samples are [1, 2, 100]. Nearest-rank p50 over three observations is the second, and
	// p99 is the third.
	if got, want := s.TPOTMsP50, 2.0; !closeEnough(got, want) {
		t.Errorf("TPOT p50 = %.3f, want %.3f (samples arrived as 100, 1, 2)", got, want)
	}
	if got, want := s.TPOTMsP99, 100.0; !closeEnough(got, want) {
		t.Errorf("TPOT p99 = %.3f, want %.3f -- a p99 that is not the slowest of three samples is not a tail",
			got, want)
	}
}

// TestTPOTPercentilesDoNotDependOnArrivalOrder states the property directly.
//
// The single-sample test that shipped with the metric could not have caught this: with one observation
// every ordering is the same ordering.
func TestTPOTPercentilesDoNotDependOnArrivalOrder(t *testing.T) {
	ascending := Summarize("off", []RawRow{deliveredRow(0, 1), deliveredRow(1, 2), deliveredRow(2, 100)})
	descending := Summarize("off", []RawRow{deliveredRow(0, 100), deliveredRow(1, 2), deliveredRow(2, 1)})

	if !closeEnough(ascending.TPOTMsP50, descending.TPOTMsP50) {
		t.Errorf("TPOT p50 depends on arrival order: %.3f vs %.3f",
			ascending.TPOTMsP50, descending.TPOTMsP50)
	}
	if !closeEnough(ascending.TPOTMsP99, descending.TPOTMsP99) {
		t.Errorf("TPOT p99 depends on arrival order: %.3f vs %.3f",
			ascending.TPOTMsP99, descending.TPOTMsP99)
	}
}

func closeEnough(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.001
}

// truncatedRow builds a row that started streaming, returned HTTP 200, then died partway.
//
// This is what the sender actually records for a stream that times out or breaks mid-flight: the status
// stays 200 because the response headers arrived, the tokens received so far are kept, and ErrorKind
// names the failure (httpsender.go stamps "timeout" or "stream"). EndUnixNanos is the moment it died,
// which for a timeout is the deadline rather than the end of any real work.
func truncatedRow(index int, kind string, spanMs float64) RawRow {
	r := deliveredRow(index, spanMs)
	r.ErrorKind = kind
	return r
}

// TestTruncatedStreamsDoNotEnterTheTPOTTail keeps a deadline out of a latency statistic.
//
// A stream killed at its 30-second timeout after two tokens reports an inter-token time of 30 seconds.
// That number describes the timeout, not the engine, and TPOT p99 is the statistic reading 1 uses to
// fail a cell that "pays for a fast first token with a slow stream". Letting deadlines into it means the
// arm most likely to be failed on stream quality is whichever arm timed out most, whatever its streams
// were actually like.
func TestTruncatedStreamsDoNotEnterTheTPOTTail(t *testing.T) {
	rows := []RawRow{
		deliveredRow(0, 5),
		deliveredRow(1, 6),
		truncatedRow(2, "timeout", 30_000),
	}
	s := Summarize("off", rows)

	if s.TPOTMsP99 > 10 {
		t.Errorf("TPOT p99 = %.1f ms; a stream that died at its deadline contributed its deadline to the tail",
			s.TPOTMsP99)
	}
}

// TestTokensFromTruncatedStreamsAreCountedAndDisclosed states the other half of the same decision.
//
// The tokens a broken stream delivered are real work the GPU did, and dropping them would understate
// what a contending tenant received -- which is the exact quantity the per-tenant share exists to
// measure. So they are counted. But a share that is partly made of failed streams means something
// different from one that is not, and this repository's rule is that a quantity may not be described by
// a cause its ledger does not establish. So the amount is recorded separately rather than folded in
// silently.
func TestTokensFromTruncatedStreamsAreCountedAndDisclosed(t *testing.T) {
	rows := []RawRow{
		deliveredRow(0, 5),
		truncatedRow(1, "stream", 6),
	}
	s := Summarize("off", rows)

	if s.OutputTokens != 4 {
		t.Errorf("OutputTokens = %d, want 4: tokens a broken stream delivered are still delivered", s.OutputTokens)
	}
	if s.OutputTokensFromFailedStreams != 2 {
		t.Errorf("OutputTokensFromFailedStreams = %d, want 2: a share partly made of failed streams must say so",
			s.OutputTokensFromFailedStreams)
	}
}

// tenantRow is deliveredRow for a named tenant.
func tenantRow(index int, tenant string, noisy bool, tpotMs float64) RawRow {
	r := deliveredRow(index, tpotMs)
	r.Tenant = tenant
	r.IsNoisy = noisy
	return r
}

// TestTPOTIsAlsoReportedPerTenant is what the pre-registered TPOT criterion actually needs.
//
// The arm-wide TPOT pools every tenant, because it is accumulated before the premium-only filter. That
// number answers "how did this arm's streams behave", which is a different question from the one reading 1
// asks: whether the PROTECTED tenant paid for its fast first token with a slow stream. On the paid
// evidence the arm-wide figure is 266.8 ms for an arm whose premium and noisy streams may be nothing alike,
// and quoting it against a premium criterion would be describing one tenant with another's number.
//
// It is free to fix: the rows are already recorded per tenant.
func TestTPOTIsAlsoReportedPerTenant(t *testing.T) {
	rows := []RawRow{
		tenantRow(0, "premium-1", false, 5),
		tenantRow(1, "premium-1", false, 6),
		tenantRow(2, "standard-noisy", true, 400),
		tenantRow(3, "standard-noisy", true, 500),
	}
	s := Summarize("off", rows)

	if got := s.TPOTMsP99ByTenant["premium-1"]; !closeEnough(got, 6) {
		t.Errorf("premium TPOT p99 = %.1f, want 6.0", got)
	}
	if got := s.TPOTMsP99ByTenant["standard-noisy"]; !closeEnough(got, 500) {
		t.Errorf("noisy TPOT p99 = %.1f, want 500.0", got)
	}
	// The arm-wide number stays, because "how did this arm's streams behave" is still a question, but it
	// must not be mistaken for either tenant's.
	if closeEnough(s.TPOTMsP99, s.TPOTMsP99ByTenant["premium-1"]) {
		t.Error("the arm-wide TPOT p99 equals the premium tenant's, so the pooling is not being tested")
	}
}
