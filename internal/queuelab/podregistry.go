package queuelab

import "sync"

// PodRegistry maps the Pod names an observer reports back to the identities the API guarantees.
//
// It is filled from the collector's own watch as Pods are observed, not from a live lookup, because by the
// time a run is reconstructed its Pods are gone -- and a live lookup would then resolve nothing, or worse,
// resolve a name that has since been reused.
//
// The reuse case is the reason this is a type rather than a map. A name is free the moment its Pod is
// deleted, and this lab creates bare Pods on the quota-guard path where the name is chosen rather than
// generated. If one name is ever seen under two UIDs, the registry stops resolving it entirely: the honest
// answer to "which of these two Pods did the exporter mean" is that nobody here knows, and picking either
// would attribute one Pod's device activity to another. That is the misattribution the whole UID requirement
// exists to prevent, and it would be reintroduced by a map that simply overwrote.
type PodRegistry struct {
	mu sync.Mutex
	// byName holds the single UID seen for a name, or "" once a second one has been seen.
	byName map[string]string
}

// Note records that a Pod with this namespace, name and UID was observed.
func (r *PodRegistry) Note(namespace, name, uid string) {
	if name == "" || uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		r.byName = map[string]string{}
	}
	key := namespace + "/" + name
	switch seen, ok := r.byName[key]; {
	case !ok:
		r.byName[key] = uid
	case seen == uid:
	default:
		// Ambiguous from here on, and permanently: a later Note carrying the first UID again must not
		// resurrect the mapping, because the ambiguity is a fact about the run and not about the last write.
		r.byName[key] = ""
	}
}

// Resolve is the PodResolver the scrape parser takes. It returns "" for a name never observed and for one
// observed under more than one identity.
//
// Both cases come back the same way on purpose. The parser counts an unresolved sample as unattributed
// either way, and that count is the evidence: a run whose observer reported Pods the collector could not
// place has an observer and a run that disagree about what was on the node.
func (r *PodRegistry) Resolve(namespace, name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byName[namespace+"/"+name]
}
