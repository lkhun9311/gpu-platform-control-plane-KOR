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

package queuelab

import (
	"fmt"
	"strings"
	"time"
)

// RenderResult renders one arm's reconstruction as text.
//
// It lives in the pure package rather than the runner so the rules that matter can be enforced by tests,
// and there are two of them now. Both were written after a published result said something the numbers did
// not support.
//
// Every admission is printed next to the execution start it does NOT imply. The published result reported
// "owner admitted in ~120 ms" while the owner did not begin executing for another 9.4 seconds, and nothing
// in the code prevented that framing.
//
// Every magnitude is printed next to the floor below which this run resolved nothing. The published result
// subtracted the protocol's own dose from the observed waste and called the 0.94 second remainder the
// control plane's cost, while the run's observations were spread across two seconds. The floor line makes
// that subtraction visibly illegitimate on the same screen as the number that invites it, which is the only
// place it can be caught by someone who is not already looking for it.
func RenderResult(res LabResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "===== RESULT (arm %s) =====\n", res.Arm)
	fmt.Fprintf(&b, "offered=%d admitted=%d executed=%d completed=%d unfinishedAtHorizon=%d\n",
		res.Offered, res.Admitted, countExecuted(res), res.Completed, res.UnfinishedAtHorizon)
	fmt.Fprintf(&b, "wastedGPUSeconds(attributable)=%.1f lowerBound=%.1f censored=%v\n",
		res.TotalWastedGPUSeconds, res.TotalWasteLowerBoundGPUSeconds, res.AnyWasteCensored)
	fmt.Fprintf(&b, "unattributedOccupancyGPUSeconds=%.1f\n", res.TotalUnattributedOccupancyGPUSeconds)
	b.WriteString(renderFloor(res.Spread))
	// The run-level occupancy and re-execution count sit in the header because that is the part of a report a
	// reader carries away; the published run put re-execution nowhere a reader would look.
	fmt.Fprintf(&b, "totalOccupancyGPUSeconds=%.1f reExecutedRows=%d\n",
		res.TotalOccupancyGPUSeconds, res.ReExecutedRows)
	if res.AnyPreemptionIneffective {
		// Stated loudly because it invalidates the natural reading of the waste number: the platform decided
		// to reclaim and the workload did not comply.
		fmt.Fprintf(&b, "PREEMPTION INEFFECTIVE: a preemption was decided but its target completed successfully%s\n",
			uncreditedLossNote(res))
	}
	if res.AnyUncreditedAttributionUnknown {
		// A silent flag helps nobody: waste=0.0 above is the honest number, but on its own it reads as "nothing
		// was lost", when in fact some occupancy has no established cause in either direction.
		//
		// This is gated on the UNCREDITED subtotal, not the combined UnattributedOccupancyGPUSeconds figure
		// printed above: that figure also counts occupancy a Succeeded stop or a completion-credited attempt
		// already explains, and printing it here would contradict this sentence for exactly that occupancy.
		fmt.Fprintf(&b, "UNATTRIBUTED OCCUPANCY: %.1f GPU-seconds could not be attributed either way"+
			" -- a preempted attempt reached no observed terminal phase, so the evidence supports neither"+
			" discarded work nor a completed run\n", res.TotalUncreditedAttributionUnknownOccupancyGPUSeconds)
	}
	fmt.Fprintf(&b, "admittedWaitP95=%s fullyObserved=%v\n",
		time.Duration(res.AdmittedWaitP95Ns), res.WaitP95FullyObserved)
	for _, o := range res.Outcomes {
		fmt.Fprintf(&b,
			"  %-10s admitted=%v executed=%v completed=%v attempts=%d reExecuted=%v preemptions=%d"+
				" preemptionIneffective=%v attributionUnknown=%v\n",
			o.Job, o.Admitted, o.Executed, o.Completed, o.Attempts, o.ReExecuted, o.Preemptions,
			o.PreemptionIneffective, o.AttributionUnknown)
		fmt.Fprintf(&b,
			"             admitLatency=%s readyLatency=%s admitToReady=%s%s\n",
			time.Duration(o.AdmitLatencyNs), time.Duration(o.ReadyLatencyNs), time.Duration(o.AdmitToReadyNs),
			censoredWaitSuffix(o))
		// Occupancy is printed against the row's declared service time because the bare number is not
		// interpretable: "81.0 for a 40 s job" is the finding, "81.0" is trivia.
		fmt.Fprintf(&b,
			"             waste=%.1f(lb %.1f%s) unattributedOccupancy=%.1f totalOccupancy=%.1f(serviceTime=%ds)\n",
			o.WastedGPUSeconds, o.WasteLowerBoundGPUSeconds, censoredMark(o.WasteCensored),
			o.UnattributedOccupancyGPUSeconds, o.TotalOccupancyGPUSeconds, o.ServiceDurationSec)
	}
	return b.String()
}

// uncreditedLossNote states the loss on a row that was both preempted ineffectively and re-executed.
//
// Without it the report trades an overclaim for an underclaim: waste=0.0 is true about the MECHANISM (the
// preemption stopped nothing) and false about the OUTCOME (a completed attempt went uncredited and the row
// re-ran from zero), and a reader takes waste=0.0 to mean nothing was lost.
func uncreditedLossNote(res LabResult) string {
	for _, o := range res.Outcomes {
		if o.PreemptionIneffective && o.ReExecuted {
			return "; that completed attempt's work was NOT credited and the row re-executed from zero," +
				" so the seconds were lost even though no waste is attributable to the preemption"
		}
	}
	return ""
}

// censoredWaitSuffix reports CensoredWaitNs only for a row never admitted by the horizon, because the field
// is a lower bound on wait in that case only and a bare number on an admitted row would misread as exact.
func censoredWaitSuffix(o WorkloadOutcome) string {
	if o.Admitted {
		return ""
	}
	return fmt.Sprintf(" censoredWait=%s(never admitted by horizon)", time.Duration(o.CensoredWaitNs))
}

// countExecuted counts rows that reached the execution-start proxy, which is deliberately reported next to
// the admitted count so the two are never conflated.
func countExecuted(res LabResult) int {
	n := 0
	for _, o := range res.Outcomes {
		if o.Executed {
			n++
		}
	}
	return n
}

// censoredMark tags a waste figure whose exact value is a lower bound.
func censoredMark(c bool) string {
	if c {
		return " censored"
	}
	return ""
}

// renderFloor states what this run could not see, and is never omitted.
//
// An absent line would be read as "no such limit", which is the opposite of what an absent spread means: a
// run whose events carried no kubelet stamp has NO bound on its intervals at all, and that is a worse
// position than a known-coarse one, not a better one. So the nil case is the louder of the two.
func renderFloor(s *ObservationSpread) string {
	if s == nil {
		return "observationFloor=UNBOUNDED -- no stop carried a kubelet timestamp to compare against," +
			" so NO interval above is bounded; treat every magnitude as unverified\n"
	}
	return fmt.Sprintf("observationFloor=%s -- differences smaller than this are NOT resolved by this run"+
		" (skew %s..%s over %d samples, kubelet stamp quantised to %s)\n",
		time.Duration(s.FloorNs), time.Duration(s.MinNs), time.Duration(s.MaxNs), s.Samples,
		time.Duration(s.QuantisationNs))
}
