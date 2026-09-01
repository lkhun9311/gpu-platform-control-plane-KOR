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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// The ownership keys are the ones the ResourceFlavor selects on, so they are functional state and not
// bookkeeping: internal/queuelab's flavor pins NodeLabels{workerLabelKey: runID} and a matching taint.
const (
	workerLabelKey = "queuelab.gpu-platform/worker"
	workerTaintKey = "queuelab.gpu-platform/dedicated"
	// The journal lives on the Node rather than in the run's namespace because piece 3's namespace teardown
	// would delete the only record of who owns the worker.
	journalKey = "queuelab.gpu-platform/ownership-journal"
	// A forced break leaves this record instead of a free Node, because nothing can prove the previous
	// process is dead and a free Node would let a second run acquire underneath a live one.
	quarantineKey = "queuelab.gpu-platform/quarantine"
	// residueKey explains a hold; it never creates one. The label and the taint are what keep Pods off the
	// node — an annotation the scheduler does not understand cannot contain a residual GPU Pod, which is the
	// same trap forceQuarantine's comment names. This record exists so the next operator can tell a run that
	// is legitimately in flight from one that finished and could not remove its namespace.
	residueKey       = "queuelab.gpu-platform/residue"
	journalSchema    = 3
	quarantineSchema = 1
	residueSchema    = 1
)

// ownerRun and ownerCanary are the two things that can hold a worker, and they recover by different routes:
// a run regenerates fixtures from its study, variant and namespace; the canary names two Pods from its id.
const (
	ownerRun    = "run"
	ownerCanary = "canary"
)

// ownerIdentity is what acquisition writes into the journal, and it is a union over the two holders rather
// than the run's own seed. Passing the seed made the canary invent a study and a variant it does not have.
type ownerIdentity struct {
	Kind      string
	TxID      string
	RunID     string
	Arm       string
	Namespace string
	// Study and Variant are set for ownerRun; CanaryID for ownerCanary.
	Study    string
	Variant  string
	CanaryID string
}

// installedTuple is exactly what this transaction wrote, so release can prove nothing moved before it
// removes anything.
type installedTuple struct {
	LabelValue  string             `json:"labelValue"`
	TaintValue  string             `json:"taintValue"`
	TaintEffect corev1.TaintEffect `json:"taintEffect"`
}

// journal identifies the transaction by a generated TxID rather than the human run id, because a reused
// run id is already a known confound and must not be able to authorise a release.
type journal struct {
	Schema  int    `json:"schema"`
	TxID    string `json:"txID"`
	RunID   string `json:"runID"`
	Arm     string `json:"arm"`
	Node    string `json:"node"`
	NodeUID string `json:"nodeUID"`
	// Kind says WHAT holds this worker, and it exists because the two holders recover differently.
	//
	// A run's objects regenerate from Study, Variant and Namespace through the fixture builder. The canary
	// builds no fixtures at all: it creates two Pods whose names are derived from its own id, inside a SHARED
	// namespace that must never be deleted. A first attempt gave the canary synthetic study and variant
	// values purely to satisfy this document's non-empty checks — which made the journal look recoverable
	// while enumerate failed on it every time, the precise defect this whole record exists to prevent.
	Kind string `json:"kind"`
	// CanaryID is set for ownerCanary only, and is enough to rebuild both probe names.
	CanaryID string `json:"canaryID,omitempty"`
	// Study, Variant and Namespace are here so that teardown can be reconstructed from the NODE alone.
	//
	// The teardown seed was documented as durable and written before the first fixture is created. It was
	// neither: it lived as a local variable handed to an in-process defer, so a crash between acquiring the
	// worker and finishing the run left fixtures on the cluster and a node marked, with nothing durable
	// naming what to delete. enumerate refuses a seed missing any of these — correctly, since an empty
	// RunID makes a flavor name that can match an unrelated run's leftovers — so recovery could not even be
	// attempted.
	//
	// They are written in the SAME patch that takes ownership, because a second write is a second place to
	// crash: the gap this closes is precisely the interval between two writes.
	// Study and Variant are set for ownerRun only.
	Study     string         `json:"study,omitempty"`
	Variant   string         `json:"variant,omitempty"`
	Namespace string         `json:"namespace"`
	TakenAt   string         `json:"takenAt"`
	Installed installedTuple `json:"installed"`
}

func encodeJournal(j journal) (string, error) {
	b, err := json.Marshal(j)
	if err != nil {
		return "", fmt.Errorf("encode journal: %w", err)
	}
	return string(b), nil
}

// decodeJournal refuses anything it does not fully understand.
//
// A state machine that decides who owns a GPU worker must not proceed from a document with fields it
// silently ignored, so unknown fields, trailing data and unknown schemas are all errors.
func decodeJournal(s string) (journal, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var j journal
	if err := dec.Decode(&j); err != nil {
		return journal{}, fmt.Errorf("decode journal: %w", err)
	}
	if dec.More() {
		return journal{}, fmt.Errorf("decode journal: trailing data after the document")
	}
	if j.Schema != journalSchema {
		return journal{}, fmt.Errorf("decode journal: schema %d is not %d", j.Schema, journalSchema)
	}
	for name, v := range map[string]string{
		"txID": j.TxID, "runID": j.RunID, "arm": j.Arm, "node": j.Node, "nodeUID": j.NodeUID,
		"kind": j.Kind, "namespace": j.Namespace,
		"installed.labelValue": j.Installed.LabelValue, "installed.taintValue": j.Installed.TaintValue,
		"installed.taintEffect": string(j.Installed.TaintEffect),
	} {
		if v == "" {
			return journal{}, fmt.Errorf("decode journal: %s is empty", name)
		}
	}
	// Checked per kind rather than across the union, because a field required of one holder and meaningless
	// to the other cannot be validated by one list without forcing somebody to write a placeholder — which is
	// exactly what the canary was made to do, and what made its journal read as recoverable when it was not.
	switch j.Kind {
	case ownerRun:
		if j.Study == "" || j.Variant == "" {
			return journal{}, fmt.Errorf("decode journal: a %s journal needs study and variant to regenerate "+
				"its fixtures, and this one has study=%q variant=%q", ownerRun, j.Study, j.Variant)
		}
	case ownerCanary:
		if j.CanaryID == "" {
			return journal{}, fmt.Errorf("decode journal: a %s journal needs canaryID to name its probe Pods",
				ownerCanary)
		}
		if j.Study != "" || j.Variant != "" {
			return journal{}, fmt.Errorf("decode journal: a %s journal carries study=%q variant=%q; the canary "+
				"builds no fixtures, so these describe nothing", ownerCanary, j.Study, j.Variant)
		}
	default:
		return journal{}, fmt.Errorf("decode journal: kind %q is neither %s nor %s", j.Kind, ownerRun, ownerCanary)
	}
	return j, nil
}

// residueLeft is one object teardown could not prove gone, in the spelling the record persists.
//
// Absence is the string absenceName produces rather than the iota, for the reason record.go already gives:
// an integer would make teardown.go's declaration order a wire format, and reordering it a silent
// compatibility break.
type residueLeft struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Absence string `json:"absence"`
}

// residueRecord is why a worker is still held after its run finished.
//
// It carries a summary AND the path of the run record, deliberately. The path alone is useless whenever the
// next run happens on another machine or in another directory — which is exactly when a human is most lost —
// so the summary must stand on its own; the path is an invitation to read more, not the payload.
type residueRecord struct {
	Schema     int           `json:"schema"`
	TxID       string        `json:"txID"`
	RunID      string        `json:"runID"`
	LeftAt     string        `json:"leftAt"`
	RecordPath string        `json:"recordPath,omitempty"`
	Left       []residueLeft `json:"left"`
}

func encodeResidue(r residueRecord) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode residue: %w", err)
	}
	return string(b), nil
}

// decodeResidue refuses anything it does not fully understand, for the same reason decodeJournal does: a
// document with fields that were silently ignored is not one to quote at an operator as fact.
//
// The CONSEQUENCE of a failure here is where the two part company. An unreadable journal is a refusal
// (reasonBadJournal), because the journal decides who owns a GPU worker. An unreadable residue record is
// not, because this record decides nothing — see residueNote. An empty Left is refused because a record
// that names nothing explains nothing, and a refusal quoting it would be worse than one that stayed silent.
func decodeResidue(s string) (residueRecord, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var r residueRecord
	if err := dec.Decode(&r); err != nil {
		return residueRecord{}, fmt.Errorf("decode residue: %w", err)
	}
	if dec.More() {
		return residueRecord{}, fmt.Errorf("decode residue: trailing data after the record")
	}
	if r.Schema != residueSchema {
		return residueRecord{}, fmt.Errorf("decode residue: schema %d is not %d", r.Schema, residueSchema)
	}
	if len(r.Left) == 0 {
		return residueRecord{}, fmt.Errorf("decode residue: left is empty, so the record explains nothing")
	}
	return r, nil
}

// The refusal reasons are named constants because the tests assert on them: an ownership state machine
// whose refusals are only free-text drifts silently when someone rewords an error.
const (
	reasonForeignOwner         = "foreign-owner"
	reasonOwnTxID              = "own-transaction-id"
	reasonMarkerWithoutJournal = "marker-without-journal"
	reasonJournalWithoutMarker = "journal-without-marker"
	reasonDuplicateTaintKey    = "duplicate-taint-key"
	reasonBadJournal           = "unreadable-journal"
	// reasonBadQuarantine is its own reason rather than reasonBadJournal because the two states send an
	// operator to different places: an unreadable journal can still be broken with -force-release, while an
	// unreadable QUARANTINE record is the one state this tool has no remaining move for, and a refusal that
	// is only free text cannot be classified by anything reading it.
	reasonBadQuarantine     = "unreadable-quarantine-record"
	reasonQuarantined       = "quarantined"
	reasonWrongNode         = "journal-names-another-node"
	reasonInstalledDiverged = "installed-values-diverged"
	reasonNotOurs           = "not-our-transaction"
	// reasonOwnershipLost is the run's own release finding nothing of its to undo, which is never the clean
	// already-released case: markers this run provably installed can only be absent because something else
	// removed them while the run was still holding the worker.
	reasonOwnershipLost = "ownership-lost-mid-run"
)

type refusal struct {
	Reason string
	Detail string
	// Cause is the error this refusal was derived from, when there was one.
	//
	// A refusal that renders its cause with %v alone is a dead end: the reason makes the state classifiable
	// but the underlying failure stops being reachable through errors.Is/errors.As, which is what a caller
	// needs to tell a decode failure from a transport one. Detail still carries the human sentence; this
	// carries the error itself.
	Cause error
}

func (r *refusal) Error() string { return r.Reason + ": " + r.Detail }

// Unwrap keeps the cause reachable. asRefusal still finds the *refusal first, because errors.As checks each
// error in the chain before unwrapping it.
func (r *refusal) Unwrap() error { return r.Cause }

func refuse(reason, format string, args ...any) error {
	return &refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// refuseCause is refuse for the refusals that were derived from another error, which must stay reachable.
func refuseCause(cause error, reason, format string, args ...any) error {
	return &refusal{Reason: reason, Detail: fmt.Sprintf(format, args...), Cause: cause}
}

// asRefusal is errors.As specialised to *refusal, kept here so the tests and the operator modes agree on
// how a refusal is recognised.
//
// It has to unwrap rather than type-assert: every call site that reports a refusal to a human wraps it with
// fmt.Errorf to say which node and which operation it came from, and a bare type assertion would stop
// recognising a refusal the moment it travelled through any of that wrapping.
func asRefusal(err error, target **refusal) bool {
	return errors.As(err, target)
}

// ownership is the Node reduced to the state the decisions actually depend on.
type ownership struct {
	NodeName   string
	NodeUID    string
	HasLabel   bool
	LabelValue string
	// Taints holds only the ownership-key taints, which are the ones the decisions compare.
	Taints []corev1.Taint
	// AllTaints holds the whole observed list, because the merge patch replaces spec.taints wholesale and
	// anything not carried over would be deleted from the Node as a side effect.
	AllTaints     []corev1.Taint
	JournalRaw    string
	Journal       journal
	JournalErr    error
	QuarantineRaw string
	// The record is kept alongside its raw text and its decode error because an unreadable record must stay
	// distinguishable from an absent one: the refusal says different things about the two.
	ResidueRaw string
	Residue    residueRecord
	ResidueErr error
}

func observe(n *corev1.Node) ownership {
	obs := ownership{NodeName: n.Name, NodeUID: string(n.UID)}
	obs.LabelValue, obs.HasLabel = n.Labels[workerLabelKey]
	obs.AllTaints = append([]corev1.Taint(nil), n.Spec.Taints...)
	for _, t := range n.Spec.Taints {
		if t.Key == workerTaintKey {
			obs.Taints = append(obs.Taints, t)
		}
	}
	obs.JournalRaw = n.Annotations[journalKey]
	obs.QuarantineRaw = n.Annotations[quarantineKey]
	if obs.JournalRaw != "" {
		obs.Journal, obs.JournalErr = decodeJournal(obs.JournalRaw)
	}
	obs.ResidueRaw = n.Annotations[residueKey]
	if obs.ResidueRaw != "" {
		obs.Residue, obs.ResidueErr = decodeResidue(obs.ResidueRaw)
	}
	return obs
}

// decideAcquire proceeds from exactly one state and refuses every other by name.
//
// Free means neither ownership key, nor a journal, nor a quarantine record: because that is the only entry
// state, the prior value of the two keys is provably absent, which is why the journal records what was
// installed rather than what to restore.
// The node object itself is deliberately NOT a parameter. obs already carries NodeName and NodeUID, and
// taking the object beside the reduction gave one fact two sources in the function that decides who owns a
// GPU worker: a caller that reduced one node and passed another would record a journal naming a node the
// decision was not made about. decideForce, decideClear and decideRelease all take obs alone.
func decideAcquire(obs ownership, id ownerIdentity, takenAt string) (journal, error) {
	txID, runID, arm := id.TxID, id.RunID, id.Arm
	if obs.QuarantineRaw != "" {
		return journal{}, refuse(reasonQuarantined,
			"node %s carries a quarantine record; clear it deliberately before any run", obs.NodeName)
	}
	if len(obs.Taints) > 1 {
		return journal{}, refuse(reasonDuplicateTaintKey,
			"node %s carries %d taints on %s", obs.NodeName, len(obs.Taints), workerTaintKey)
	}
	hasMarker := obs.HasLabel || len(obs.Taints) == 1
	switch {
	case obs.JournalRaw != "" && obs.JournalErr != nil:
		return journal{}, refuse(reasonBadJournal, "node %s: %v", obs.NodeName, obs.JournalErr)
	case obs.JournalRaw != "" && !hasMarker:
		return journal{}, refuse(reasonJournalWithoutMarker,
			"node %s carries a journal for tx %q but no marker", obs.NodeName, obs.Journal.TxID)
	case obs.JournalRaw != "" && obs.Journal.TxID == txID:
		return journal{}, refuse(reasonOwnTxID,
			"node %s is already held by this transaction %s", obs.NodeName, txID)
	case obs.JournalRaw != "":
		// Run id, transaction id and timestamp all come out of the node's own journal, and this refusal is
		// printed to a terminal by the run path exactly as inspectWorker prints its own.
		return journal{}, refuse(reasonForeignOwner,
			"node %s is held by run %q under tx %q since %q%s",
			obs.NodeName, obs.Journal.RunID, obs.Journal.TxID, obs.Journal.TakenAt, residueNote(obs))
	case hasMarker:
		return journal{}, refuse(reasonMarkerWithoutJournal,
			"node %s carries a queuelab marker with no journal; it cannot be released safely by this tool",
			obs.NodeName)
	}
	return journal{
		Schema:    journalSchema,
		TxID:      txID,
		RunID:     runID,
		Arm:       arm,
		Node:      obs.NodeName,
		NodeUID:   obs.NodeUID,
		Kind:      id.Kind,
		CanaryID:  id.CanaryID,
		Study:     id.Study,
		Variant:   id.Variant,
		Namespace: id.Namespace,
		TakenAt:   takenAt,
		Installed: installedTuple{
			LabelValue:  runID,
			TaintValue:  runID,
			TaintEffect: corev1.TaintEffectNoSchedule,
		},
	}, nil
}

// residueNote is what a residue record adds to a foreign-owner refusal, and the only thing it adds anywhere.
//
// It returns "" when there is no record, so a refusal on a node without one is byte-for-byte what it was
// before this existed. An unreadable record degrades to saying so: this document carries no safety weight,
// and an informational field that invents a new failure mode is worse than no field at all.
//
// The finalizer warning is repeated here rather than left to the run record, because this refusal is what an
// operator reads at the exact moment they are most tempted to strip a stuck namespace's finalizer — and the
// teardown design forbids ever offering that as a fix, since it orphans the contents and every absence check
// afterwards reports clean over objects that are still running.
//
// Every field decoded out of the record is quoted, for the reason inspectWorker states about the journal:
// decoding a hostile string does not make it safe. All of this came out of a Node annotation, this refusal is
// printed straight to an operator's terminal by both the run path and inspectWorker, and the last line below
// hands that operator a command to run — so an unescaped kind or record path could rewrite the instructions
// printed around it into a different, legitimate-looking one. The node name is the one thing here this tool
// did not read out of an annotation, so it stays unquoted like every other node name this package prints.
func residueNote(obs ownership) string {
	if obs.ResidueRaw == "" {
		return ""
	}
	if obs.ResidueErr != nil {
		return fmt.Sprintf("; it also carries a residue record that could not be read: %v", obs.ResidueErr)
	}
	var b strings.Builder
	fmt.Fprintf(&b, ";\n  that run's teardown left %d object(s) behind at %q, so the worker is held "+
		"deliberately and its GPUs may still be in use:", len(obs.Residue.Left), obs.Residue.LeftAt)
	for _, l := range obs.Residue.Left {
		fmt.Fprintf(&b, "\n    %q %q (%q)", l.Kind, l.Name, l.Absence)
	}
	if obs.Residue.RecordPath != "" {
		fmt.Fprintf(&b, "\n  full record: %q", obs.Residue.RecordPath)
	}
	fmt.Fprintf(&b, "\n  do NOT strip a stuck namespace's finalizer; run: queuelabrun -inspect-worker -worker %s",
		obs.NodeName)
	return b.String()
}

type releaseAction int

const (
	// releaseRestore removes exactly the two values this transaction installed, plus its journal.
	releaseRestore releaseAction = iota
	// releaseAlreadyDone is a success: nothing of ours is on the Node, so there is nothing to undo.
	releaseAlreadyDone
)

// verifyInstalled proves the two values this transaction wrote are still exactly what it wrote.
//
// Only those values are compared: unrelated labels and taints, including the operator's own unhealthy
// taint, are benign drift and must not invalidate an otherwise valid run.
func verifyInstalled(obs ownership, j journal) error {
	if !obs.HasLabel || obs.LabelValue != j.Installed.LabelValue {
		return refuse(reasonInstalledDiverged, "label %s is %q, this transaction installed %q",
			workerLabelKey, obs.LabelValue, j.Installed.LabelValue)
	}
	if len(obs.Taints) != 1 {
		return refuse(reasonInstalledDiverged, "node carries %d taints on %s, this transaction installed 1",
			len(obs.Taints), workerTaintKey)
	}
	if obs.Taints[0].Value != j.Installed.TaintValue || obs.Taints[0].Effect != j.Installed.TaintEffect {
		// Both effects are escaped, not just the values. decodeJournal checks only that installed.taintEffect
		// is non-empty — there is no enum validation — so the journal's copy is node-controlled bytes like
		// everything else in that document, and inspectWorker prints this refusal directly above the
		// -force-release command it invites the operator to copy.
		return refuse(reasonInstalledDiverged, "taint %s is %q/%q, this transaction installed %q/%q",
			workerTaintKey, obs.Taints[0].Value, string(obs.Taints[0].Effect),
			j.Installed.TaintValue, string(j.Installed.TaintEffect))
	}
	if obs.NodeUID != j.NodeUID {
		return refuse(reasonWrongNode, "node UID is %s, the journal names %s", obs.NodeUID, j.NodeUID)
	}
	return nil
}

// withOwnershipTaint returns the taint list to patch, built from the observed list.
//
// The merge patch replaces the array wholesale, so anything not carried over here is deleted from the Node.
func withOwnershipTaint(observed []corev1.Taint, runID string) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(observed)+1)
	for _, t := range observed {
		if t.Key != workerTaintKey {
			out = append(out, t)
		}
	}
	return append(out, corev1.Taint{
		Key: workerTaintKey, Value: runID, Effect: corev1.TaintEffectNoSchedule,
	})
}

// withoutOwnershipTaint removes only this experiment's taint key, leaving every other taint untouched.
func withoutOwnershipTaint(observed []corev1.Taint) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(observed))
	for _, t := range observed {
		if t.Key != workerTaintKey {
			out = append(out, t)
		}
	}
	return out
}

// quarantine is what a forced break leaves behind instead of a free node.
//
// No flag can prove the previous process is dead, so the break records what it removed and blocks
// acquisition until an operator deliberately clears it in a second step.
type quarantine struct {
	Schema         int            `json:"schema"`
	QuarantineID   string         `json:"quarantineID"`
	ForcedAt       string         `json:"forcedAt"`
	Node           string         `json:"node"`
	NodeUID        string         `json:"nodeUID"`
	PriorJournal   string         `json:"priorJournal"`
	ObservedLabel  string         `json:"observedLabel"`
	ObservedTaints []corev1.Taint `json:"observedTaints,omitempty"`
}

func encodeQuarantine(q quarantine) (string, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return "", fmt.Errorf("encode quarantine: %w", err)
	}
	return string(b), nil
}

// decodeQuarantine is as strict as decodeJournal, and for the same reason.
//
// The quarantine record is the only surviving evidence of who held a forcibly broken node, and it is what
// decideClear matches the operator's -quarantine-id against, so a document with anything after it is one
// this tool does not fully understand and must not act on.
func decodeQuarantine(s string) (quarantine, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var q quarantine
	if err := dec.Decode(&q); err != nil {
		return quarantine{}, fmt.Errorf("decode quarantine: %w", err)
	}
	if dec.More() {
		return quarantine{}, fmt.Errorf("decode quarantine: trailing data after the document")
	}
	if q.Schema != quarantineSchema {
		return quarantine{}, fmt.Errorf("decode quarantine: schema %d is not %d", q.Schema, quarantineSchema)
	}
	if q.QuarantineID == "" {
		return quarantine{}, fmt.Errorf("decode quarantine: quarantineID is empty")
	}
	return q, nil
}

// decideForce refuses to force a node that is already quarantined and requires the operator to confirm the
// target by its UID.
func decideForce(obs ownership, nodeUID, quarantineID, forcedAt string) (quarantine, error) {
	if obs.QuarantineRaw != "" {
		return quarantine{}, refuse(reasonQuarantined,
			"node %s is already quarantined; clear that record instead of forcing again", obs.NodeName)
	}
	if obs.NodeUID != nodeUID {
		return quarantine{}, refuse(reasonWrongNode,
			"node %s has UID %s, you confirmed %s", obs.NodeName, obs.NodeUID, nodeUID)
	}
	return quarantine{
		Schema:         quarantineSchema,
		QuarantineID:   quarantineID,
		ForcedAt:       forcedAt,
		Node:           obs.NodeName,
		NodeUID:        obs.NodeUID,
		PriorJournal:   obs.JournalRaw,
		ObservedLabel:  obs.LabelValue,
		ObservedTaints: obs.Taints,
	}, nil
}

// decideClear removes only the exact record the operator names, on the exact object it was written about.
//
// The quarantine id alone is not enough. decideForce records the node NAME and the node UID precisely
// because a record is a document that can be copied onto another Node, and because a Node deleted and
// recreated under the same name is a different machine wearing the same label. Clearing is the step that
// makes a node acquirable again, so it must confirm it is unblocking the object that was actually broken,
// the same way decideForce required the operator to confirm the target it was breaking.
func decideClear(obs ownership, quarantineID string) error {
	if obs.QuarantineRaw == "" {
		return refuse(reasonNotOurs, "node %s carries no quarantine record", obs.NodeName)
	}
	q, err := decodeQuarantine(obs.QuarantineRaw)
	if err != nil {
		return refuseCause(err, reasonBadQuarantine, "node %s: %v", obs.NodeName, err)
	}
	if q.QuarantineID != quarantineID {
		return refuse(reasonNotOurs, "node %s carries quarantine %s, you named %s",
			obs.NodeName, q.QuarantineID, quarantineID)
	}
	if q.Node != obs.NodeName || q.NodeUID != obs.NodeUID {
		return refuse(reasonWrongNode,
			"quarantine %s was recorded for node %s (uid %s), this node is %s (uid %s)",
			quarantineID, q.Node, q.NodeUID, obs.NodeName, obs.NodeUID)
	}
	return nil
}

// decideRelease says what release may do, and refusing is a run-invalidating outcome rather than a warning.
//
// Restoring over a value that moved would be a second act of damage, so divergence refuses and hands the
// node to the operator modes instead.
func decideRelease(obs ownership, txID string) (releaseAction, error) {
	if obs.QuarantineRaw != "" {
		return releaseRestore, refuse(reasonQuarantined,
			"node %s was force-released under a quarantine record while this run held it", obs.NodeName)
	}
	if obs.JournalRaw == "" {
		if obs.HasLabel || len(obs.Taints) > 0 {
			return releaseRestore, refuse(reasonMarkerWithoutJournal,
				"node %s carries a marker with no journal", obs.NodeName)
		}
		return releaseAlreadyDone, nil
	}
	if obs.JournalErr != nil {
		return releaseRestore, refuse(reasonBadJournal, "node %s: %v", obs.NodeName, obs.JournalErr)
	}
	if obs.Journal.TxID != txID {
		return releaseRestore, refuse(reasonNotOurs,
			"node %s is held by tx %s, not %s", obs.NodeName, obs.Journal.TxID, txID)
	}
	if err := verifyInstalled(obs, obs.Journal); err != nil {
		return releaseRestore, err
	}
	return releaseRestore, nil
}
