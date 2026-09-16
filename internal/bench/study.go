package bench

import (
	"fmt"
	"slices"
)

// A Study is one pre-registered experiment: the arms it admits, and the order a report prints them in.
//
// Arm names used to live in one closed map shared by every experiment this repository runs, which was
// fine while there was one experiment. It stops being fine the moment a second one wants a name the
// first already used. M5-b's "off" was the gateway with its admission guard disabled. The
// price-of-protection run's control is the engine at its own default batch budget with no gateway in
// the path at all. Those are different conditions that would produce comparable-looking rows, and the
// pre-registration's own wording invites the confusion by calling the new control "off" as well.
//
// So an arm belongs to a study, the study travels with the evidence, and a report refuses to pool rows
// from two of them. Guarding by name would only work until someone picked a name that collided.
type Study struct {
	// ID is the immutable identifier raw evidence carries.
	//
	// Changing it orphans every record already written, so it is dated rather than versioned by
	// meaning: a study that needs different arms is a different study.
	ID string
	// Arms are the canonical arm names, in the order a report prints them.
	//
	// Order is a property of the study rather than of the names. The price-of-protection sweep wants
	// its isolated ceiling and its control first and its eight cells after, which no sort of those
	// strings produces.
	Arms []string
	// Arrivals is the arrival model the study's pre-registration fixed, or empty where none was registered.
	//
	// Two ladders can carry the same arm names, criterion and rungs and differ only in how their traces were
	// generated, and nothing in a trace file says which. So the model is recorded here, gen-trace refuses
	// flags of the other model for a study that declares one, and hack/m5c-matrix.sh asks this registry
	// rather than keeping a second copy of the answer in a shell variable.
	Arrivals ArrivalModel
}

// ArrivalModel names how a study's traces assign arrival times, which decides the gen-trace flags that may build them.
type ArrivalModel string

const (
	// ArrivalsWeighted draws every gap at one total rate and picks each arrival's tenant by weight.
	ArrivalsWeighted ArrivalModel = "weighted"
	// ArrivalsIndependent gives each tenant its own Poisson process keyed by its name; see TenantSpec.RatePerSec.
	ArrivalsIndependent ArrivalModel = "independent"
)

const (
	// StudyM5BGateway is the four-condition gateway experiment M5-b measured: an isolated baseline, the
	// guard disabled, a static admission cap, and the KV-occupancy guard.
	//
	// Evidence written before studies existed carries no identifier at all, and is read as this one.
	StudyM5BGateway = "m5b-gateway-v1"
	// StudyPriceOfProtection is the engine-configuration sweep pre-registered in
	// docs/superpowers/specs/2026-09-05-the-price-of-protection.md.
	//
	// No gateway is in the path. The factors are vLLM's batch budget and its scheduling policy.
	StudyPriceOfProtection = "price-of-protection-2026-09-05"
	// StudySharingMatrix is the M5-c topology matrix pre-registered in
	// docs/superpowers/specs/2026-09-10-does-splitting-the-card-buy-protection.md.
	//
	// Its arms are TOPOLOGIES, not admission modes, and giving it a study of its own is what lets the report
	// tell them apart. hack/m5c-matrix.sh used to replay every arm as "off" -- the admission vocabulary's
	// name for a disabled guard -- because that was the only arm name the harness would accept for it, and
	// it then had to ship a README telling readers never to run `benchharness report` over the evidence,
	// since pooling would collapse three topologies into one row. Evidence that has to arrive with a warning
	// against using the tool that reads it is evidence one step from being read wrong.
	StudySharingMatrix = "sharing-matrix-2026-09-10"
	// StudyThroughputLadder is the capacity ladder pre-registered in
	// docs/superpowers/specs/2026-09-13-what-the-split-costs-in-throughput.md.
	//
	// It measures the same two topologies as StudySharingMatrix at four offered premium loads, and it is a
	// separate study rather than more repetitions of that one for a reason worth stating: repetitions of the
	// sharing matrix are POOLED, and pooling two different offered loads into one arm summary would produce
	// a p99 for a load that was never offered. Its arm names carry the rung for the same reason -- see
	// ThroughputLadderArm.
	StudyThroughputLadder = "throughput-ladder-2026-09-13"
	// StudyThroughputLadderDown is the second capacity ladder, pre-registered in
	// docs/superpowers/specs/2026-09-13-the-ladder-has-to-search-downward.md.
	//
	// Same arms, same criterion, same readings, different RUNGS: the first ladder climbed from the load the
	// sharing matrix answered, and both topologies already missed the target there by a factor of seven, so
	// it could return nothing but "no qualified operating point at or above the bottom rung". This one places
	// its rungs BELOW that load.
	//
	// A separate study rather than new rungs on the old one for the reason the old one's arm names carry
	// their rung: rung01 means a different offered load in each, and evidence that pooled them would report a
	// p99 for a load nobody offered. The trace-identity refusal would also catch it, but a refusal is not the
	// same as a reader being able to tell two experiments apart.
	StudyThroughputLadderDown = "throughput-ladder-down-2026-09-13"
	// StudyThroughputLadderIndependent is the downward ladder's rungs re-registered under independent
	// arrivals, in docs/superpowers/specs/2026-09-15-a-ladder-whose-contender-holds-still.md.
	//
	// The down ladder solved a contender weight for every rung to hold 139 offers, and the count held while the
	// contender's SCHEDULE changed at every rung, so a rung-to-rung comparison moved two things at once. Here
	// the contender's rows are byte-identical at every rung. It is a separate study because its rung01 trace is
	// not the down ladder's rung01 trace, and pooling the two would report a p99 over two contender schedules.
	StudyThroughputLadderIndependent = "throughput-ladder-independent-2026-09-15"
)

// The factors the price-of-protection sweep crosses.
//
// 8192 is absent deliberately: the scheduler microtest measured both policies degenerating there
// (11.83x), and 256 is present because the pattern between 512 and 2048 is non-monotone and
// unexplained, so a point below the only budget that worked is worth more than a fourth point above.
var (
	priceOfProtectionBudgets  = []int{256, 512, 1024, 2048}
	priceOfProtectionPolicies = []string{"fcfs", "priority"}
)

// PremiumTenant is the tenant whose tail every pre-registered criterion is about.
//
// Named once because a literal spelled at each use is the copy that eventually differs by a hyphen, and
// this repository has already paid twice for hand-kept copies of a tenant list drifting apart.
const PremiumTenant = "premium-1"

// NoisyTenant is the contending tenant whose work the positive readings require to survive.
//
// It was spelled as a literal in the trace builder and nowhere else, which was fine while nothing read it
// back. The price-of-protection readings compare this tenant's output share against the control's, so the
// name is now load-bearing in two places and belongs beside PremiumTenant for the reason stated above it.
const NoisyTenant = "standard-noisy"

// ArmR1 is the isolated premium baseline every study measures as its ceiling.
//
// It replays the same trace with the contending tenant filtered out, so its record count legitimately
// differs from every other arm's and identity checks have to exclude it.
const ArmR1 = "R1"

// ArmShared and the two sharing arms are the M5-c matrix's topologies.
//
// They live here beside the other studies' arm names, and not in the evaluator that reads them, because the
// registry below has to name them: a study whose arms are declared in the file that scores them cannot be
// registered without that file, and the runner would then be free to write an arm nobody validates.
//
// The spellings are the ones hack/m5c-matrix.sh puts in its output paths, because those paths are what a
// reader has in front of them when they run the report.
const (
	ArmShared      = "shared"
	ArmTimeSlicing = "timeSlicing"
	ArmMPS         = "mps"
)

// ArmDefaultFCFS is the price-of-protection control: the engine at its own default batch budget under
// first-come-first-served, which is what an operator who configures nothing gets.
//
// It is NOT named "off" even though the pre-registration's readings use that word for it. M5-b's "off"
// arm ran through a gateway, and giving the two the same name would make raw evidence from a
// no-gateway run indistinguishable from evidence that crossed a proxy hop.
const ArmDefaultFCFS = "default-fcfs"

// priceOfProtectionArms generates the sweep's arm names from the factors rather than listing them.
//
// A hand-written list of ten strings is a fifth copy of the factor definitions, and this repository has
// already paid twice for hand-kept lists drifting apart. The budget is zero-padded so that lexical
// order is numeric order: b256 would sort after b1024 and put the table in an order no reader expects.
func priceOfProtectionArms() []string {
	arms := make([]string, 0, 2+len(priceOfProtectionBudgets)*len(priceOfProtectionPolicies))
	arms = append(arms, ArmR1, ArmDefaultFCFS)
	for _, budget := range priceOfProtectionBudgets {
		for _, policy := range priceOfProtectionPolicies {
			arms = append(arms, PriceOfProtectionArm(budget, policy))
		}
	}
	return arms
}

// PriceOfProtectionArm is the canonical name of one cell of the sweep.
//
// "mbt" is max_num_batched_tokens, spelled short because the name sits in a fixed-width report column
// and spelled consistently because a reader who has to decode two abbreviations for one factor will
// eventually decode one of them wrong.
func PriceOfProtectionArm(budget int, policy string) string {
	return fmt.Sprintf("mbt-%04d-%s", budget, policy)
}

// ThroughputLadderArm is the canonical name of one cell of the capacity ladder: one topology at one rung.
//
// The rung is IN THE ARM NAME, and that is the point rather than a convenience. An arm summary pools every
// row carrying its name, so two rungs sharing an arm name would be averaged into a p99 for an offered load
// that was never offered -- a plausible wrong number of exactly the class this repository's rules put above
// every other failure. Making the rung part of the identity makes that pooling unrepresentable.
//
// Zero-padded so that lexical order is numeric order, which is what the report's arm column is sorted by.
func ThroughputLadderArm(rung int, topology string) string {
	return fmt.Sprintf("rung%02d-%s", rung, topology)
}

// IsIsolatedBaseline says whether an arm name is a study's uncontended premium baseline.
//
// It exists because "is this R1" was spelled as a literal string comparison in two places that decide
// something load-bearing: gen-trace filters the contending tenant out of the baseline's trace, and the
// trace-identity check exempts the baseline because its row count legitimately differs. The ladder's
// baseline is called rung04-R1, so both would have looked straight past it -- producing a "baseline" that
// carried the contender, and a refusal that the traces disagree. Neither would have announced itself.
//
// Asking a function rather than comparing a literal is what makes a third study's baseline work by
// construction instead of by somebody remembering these two call sites.
func IsIsolatedBaseline(arm string) bool {
	if arm == ArmR1 {
		return true
	}
	_, topology, ok := parseLadderArmName(arm)
	return ok && topology == ArmR1
}

// ArmComparisonGroup names the set of arms an arm is compared against.
//
// Every study but one offers a single load, so all its arms are one group and the name is empty. The
// capacity ladder offers a different load per rung by design, so its group is the rung: the two topologies
// of rung 1 must replay the same trace as each other and must NOT replay rung 2's -- a ladder whose rungs
// agreed would be four measurements of one load.
//
// The report's trace-identity refusal is written against one group and was correct for every study that
// existed when it was written. It refused the ladder's own evidence the first time the ladder ran.
func ArmComparisonGroup(arm string) string {
	if rung, _, ok := parseLadderArmName(arm); ok {
		return fmt.Sprintf("rung%02d", rung)
	}
	return ""
}

// throughputLadderRungs is how many rungs the pre-registration lists.
//
// The rung PARAMETERS -- the arrival rate and the tenant weights -- live in the runner and in the
// pre-registration, not here: this package scores evidence and never generates load. What it needs is only
// how many names to admit.
const throughputLadderRungs = 4

// throughputLadderArms generates the ladder's arm names from the rungs rather than listing sixteen strings.
//
// Every rung admits both topologies and the isolated baseline, even though R1 is bought at ONE rung. Which
// rung that is depends on where the stopping rule fires, which is not known until the run happens, and a
// registry that admitted R1 at only one rung would have to be edited once the ladder chose -- an edit to the
// instrument after seeing data, which is the thing the pre-registration exists to prevent.
func throughputLadderArms() []string {
	arms := make([]string, 0, throughputLadderRungs*3)
	for rung := 1; rung <= throughputLadderRungs; rung++ {
		arms = append(arms,
			ThroughputLadderArm(rung, ArmShared),
			ThroughputLadderArm(rung, ArmTimeSlicing),
			ThroughputLadderArm(rung, ArmR1))
	}
	return arms
}

// studies is the registry every arm name is validated against.
var studies = map[string]Study{
	StudyM5BGateway: {
		ID:   StudyM5BGateway,
		Arms: []string{ArmR1, "off", "static-cap", "kv-aware"},
	},
	StudyPriceOfProtection: {
		ID:   StudyPriceOfProtection,
		Arms: priceOfProtectionArms(),
	},
	StudySharingMatrix: {
		ID:   StudySharingMatrix,
		Arms: []string{ArmR1, ArmShared, ArmTimeSlicing, ArmMPS},
	},
	StudyThroughputLadder: {
		ID:       StudyThroughputLadder,
		Arms:     throughputLadderArms(),
		Arrivals: ArrivalsWeighted,
	},
	StudyThroughputLadderDown: {
		ID:       StudyThroughputLadderDown,
		Arms:     throughputLadderArms(),
		Arrivals: ArrivalsWeighted,
	},
	StudyThroughputLadderIndependent: {
		ID:       StudyThroughputLadderIndependent,
		Arms:     throughputLadderArms(),
		Arrivals: ArrivalsIndependent,
	},
}

// CanonicalStudyID resolves an identifier as recorded to the identifier it means.
//
// Evidence written before the study field existed carries an empty string, and LookupStudy already reads
// that as the gateway experiment. Comparisons have to use the SAME normalization, or a file labelled
// "m5b-gateway-v1" and an unlabelled file from the same experiment are refused as different studies --
// which is what happened, because the pooling check compared the raw strings.
func CanonicalStudyID(id string) string {
	if id == "" {
		return StudyM5BGateway
	}
	return id
}

// LookupStudy returns the study with this ID.
//
// An empty ID is the evidence written before studies existed, and resolves to the M5-b gateway
// experiment, which is the only thing it can be.
func LookupStudy(id string) (Study, bool) {
	if id == "" {
		id = StudyM5BGateway
	}
	s, ok := studies[id]
	return s, ok
}

// KnownStudyIDs lists the registered studies, for a refusal that has to name the alternatives.
func KnownStudyIDs() []string {
	// DERIVED from the registry and then sorted, rather than hand-listed.
	//
	// The hand-written version said it was a list "because a refusal message whose order changes between
	// runs is a refusal message that cannot be tested", which is a real requirement and the wrong fix for
	// it. Sorting satisfies it too, and a hand-kept copy of the registry does not stay a copy: registering
	// the sharing matrix left this returning two of three studies, so the refusal for a mistyped study ID
	// would have listed the alternatives and omitted the one the operator was reaching for. This repository
	// has paid twice for hand-kept lists drifting from what they list.
	ids := make([]string, 0, len(studies))
	for id := range studies {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Admits reports whether arm is one of this study's conditions.
func (s Study) Admits(arm string) bool {
	return slices.Contains(s.Arms, arm)
}

// ArmColumnWidth is the width of the report's arm column: the longest arm name any study defines.
//
// It was the literal 12, which fitted M5-b's four names and silently misaligned anything longer --
// "mbt-0512-priority" is 17 characters and would have pushed every column after it out of line in the
// one table a reader actually looks at. Derived from the registry so that adding a study cannot break
// the report by a mechanism nobody thinks to check.
var ArmColumnWidth = widestArmName()

func widestArmName() int {
	w := len("arm")
	for _, s := range studies {
		for _, a := range s.Arms {
			if len(a) > w {
				w = len(a)
			}
		}
	}
	return w
}
