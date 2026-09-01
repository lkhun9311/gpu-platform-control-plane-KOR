package queuelab

import "testing"

// The ordinary mapping, so the refusals below are refusals of something.
func TestPodRegistryResolvesWhatItSaw(t *testing.T) {
	var r PodRegistry
	r.Note("queuelab-r1", "a2-borrow-x7k2p", "victim-uid")
	if got := r.Resolve("queuelab-r1", "a2-borrow-x7k2p"); got != "victim-uid" {
		t.Fatalf("resolve = %q, want victim-uid", got)
	}
	if got := r.Resolve("queuelab-r1", "never-seen"); got != "" {
		t.Fatalf("a name never observed resolved to %q", got)
	}
	if got := r.Resolve("other-namespace", "a2-borrow-x7k2p"); got != "" {
		t.Fatalf("the same name in another namespace resolved to %q", got)
	}
}

// The case the whole UID requirement exists for: one name, two identities. Picking either would attribute
// one Pod's device activity to another.
//
// Mutation that turns this red: overwrite on conflict instead of blanking, or keep the first UID.
func TestAReusedNameStopsResolvingAltogether(t *testing.T) {
	var r PodRegistry
	r.Note("serving", "stub-llm", "first-uid")
	r.Note("serving", "stub-llm", "second-uid")
	if got := r.Resolve("serving", "stub-llm"); got != "" {
		t.Fatalf("a name seen under two identities resolved to %q; one Pod's device activity would be "+
			"credited to another", got)
	}
	// And it stays ambiguous. A later sighting of the first UID must not resurrect the mapping, because the
	// ambiguity is a fact about the run rather than about the last write.
	r.Note("serving", "stub-llm", "first-uid")
	if got := r.Resolve("serving", "stub-llm"); got != "" {
		t.Fatalf("the mapping came back as %q after a third sighting", got)
	}
}

// Repeats of the same identity are the ordinary case -- a Pod is observed many times across a run -- and
// must not look like reuse.
func TestSeeingTheSamePodAgainIsNotAConflict(t *testing.T) {
	var r PodRegistry
	for range 5 {
		r.Note("queuelab-r1", "b1-owner-m4t9q", "owner-uid")
	}
	if got := r.Resolve("queuelab-r1", "b1-owner-m4t9q"); got != "owner-uid" {
		t.Fatalf("resolve = %q after five identical sightings", got)
	}
}

// Partial observations are dropped rather than recorded as a mapping to nothing.
func TestIncompleteSightingsAreIgnored(t *testing.T) {
	var r PodRegistry
	r.Note("queuelab-r1", "", "some-uid")
	r.Note("queuelab-r1", "a-name", "")
	if got := r.Resolve("queuelab-r1", "a-name"); got != "" {
		t.Fatalf("a sighting with no UID produced the mapping %q", got)
	}

	// And a partial sighting must not POISON the name. Recording it would write an empty UID, which the
	// conflict rule then reads as ambiguity -- so a Pod observed once before its UID was available would be
	// permanently unresolvable, and every one of its samples would count as unattributed for the rest of the
	// run. That is the difference between dropping an incomplete observation and recording it as an unknown.
	r.Note("queuelab-r1", "a-name", "the-real-uid")
	if got := r.Resolve("queuelab-r1", "a-name"); got != "the-real-uid" {
		t.Fatalf("resolve = %q; an earlier partial sighting poisoned the name against a later complete one", got)
	}
}

// It is written from a watch loop and read from a scrape loop, so it has to be safe to use from both.
func TestPodRegistryIsSafeUnderConcurrentUse(t *testing.T) {
	var r PodRegistry
	done := make(chan struct{})
	go func() {
		for i := range 500 {
			_ = i
			r.Note("ns", "p", "uid")
		}
		close(done)
	}()
	for range 500 {
		r.Resolve("ns", "p")
	}
	<-done
	if got := r.Resolve("ns", "p"); got != "uid" {
		t.Fatalf("resolve = %q after concurrent use", got)
	}
}
