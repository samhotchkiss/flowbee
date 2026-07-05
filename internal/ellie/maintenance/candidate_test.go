package maintenance

import (
	"context"
	"testing"
)

func TestPairCandidateKeyStableRegardlessOfInputOrder(t *testing.T) {
	ab := mustPair(t, Member{ID: "a", ContentHash: "ha"}, Member{ID: "b", ContentHash: "hb"})
	ba := mustPair(t, Member{ID: "b", ContentHash: "hb"}, Member{ID: "a", ContentHash: "ha"})

	if ab.Key != ba.Key {
		t.Fatalf("pair key changed with input order: %q != %q", ab.Key, ba.Key)
	}
	if ab.Key == `pair:["a","b"]` {
		return
	}
	t.Fatalf("unexpected canonical pair key %q", ab.Key)
}

func TestClusterCandidateKeyStableRegardlessOfInputOrder(t *testing.T) {
	abc := mustCluster(t,
		Member{ID: "a", ContentHash: "ha"},
		Member{ID: "b", ContentHash: "hb"},
		Member{ID: "c", ContentHash: "hc"},
	)
	cab := mustCluster(t,
		Member{ID: "c", ContentHash: "hc"},
		Member{ID: "a", ContentHash: "ha"},
		Member{ID: "b", ContentHash: "hb"},
	)

	if abc.Key != cab.Key {
		t.Fatalf("cluster key changed with input order: %q != %q", abc.Key, cab.Key)
	}
}

func TestHashComparisonsAreExactPerMember(t *testing.T) {
	c := mustCluster(t,
		Member{ID: "a", ContentHash: "ha"},
		Member{ID: "b", ContentHash: "hb"},
	)

	if !ContentHashesMatch(c, []Member{{ID: "b", ContentHash: "hb"}, {ID: "a", ContentHash: "ha"}}) {
		t.Fatal("same member ids and hashes should match despite input order")
	}
	if ContentHashesMatch(c, []Member{{ID: "a", ContentHash: "hb"}, {ID: "b", ContentHash: "ha"}}) {
		t.Fatal("same hash set assigned to different member ids must not match")
	}
	if ContentHashesMatch(c, []Member{{ID: "a", ContentHash: "ha"}, {ID: "b", ContentHash: "changed"}}) {
		t.Fatal("one changed hash must make the candidate eligible")
	}
	if ContentHashesMatch(c, []Member{{ID: "a", ContentHash: "ha"}}) {
		t.Fatal("removed cluster member must make the candidate eligible")
	}
	if ContentHashesMatch(c, []Member{{ID: "a", ContentHash: "ha"}, {ID: "b", ContentHash: "hb"}, {ID: "c", ContentHash: "hc"}}) {
		t.Fatal("added cluster member must make the candidate eligible")
	}
}

func TestCompletedAndFailedStatuses(t *testing.T) {
	for _, status := range []ResultStatus{ResultSuccess, ResultNoOp, ResultRefusal, ResultNonActionable} {
		if !IsCompletedStatus(status) {
			t.Fatalf("%s should count as completed", status)
		}
	}
	for _, status := range []ResultStatus{ResultFailed, ResultCanceled, ResultTimedOut} {
		if IsCompletedStatus(status) {
			t.Fatalf("%s should not count as completed", status)
		}
	}
}

func TestFilterEligibleCountsUnchangedSkipsAndLLMSent(t *testing.T) {
	ab := mustPair(t, Member{ID: "a", ContentHash: "ha"}, Member{ID: "b", ContentHash: "hb"})
	ac := mustPair(t, Member{ID: "a", ContentHash: "ha"}, Member{ID: "c", ContentHash: "hc"})
	checker := mapChecker{done: map[string]bool{ab.Key: true}}

	eligible, stats, err := FilterEligible(context.Background(), checker, "store", SweepContradiction, []Candidate{ab, ac})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(eligible) != 1 || eligible[0].Key != ac.Key {
		t.Fatalf("eligible=%v, want only ac", eligible)
	}
	if stats.CandidatesGenerated != 2 || stats.SkippedUnchanged != 1 || stats.SentToLLM != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}

type mapChecker struct {
	done map[string]bool
}

func TestNormalizeCandidateCanonicalizesDirectCandidates(t *testing.T) {
	raw := Candidate{
		Kind: CandidatePair,
		Members: []Member{
			{ID: "b", ContentHash: "hb"},
			{ID: "a", ContentHash: "ha"},
		},
	}
	got, err := NormalizeCandidate(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := mustPair(t, Member{ID: "a", ContentHash: "ha"}, Member{ID: "b", ContentHash: "hb"})
	if got.Key != want.Key {
		t.Fatalf("key=%q, want %q", got.Key, want.Key)
	}
	if got.Members[0].ID != "a" || got.Members[1].ID != "b" {
		t.Fatalf("members not canonical: %+v", got.Members)
	}
}

func TestNormalizeCandidateRejectsMismatchedKey(t *testing.T) {
	_, err := NormalizeCandidate(Candidate{
		Kind: CandidatePair,
		Key:  `pair:["x","y"]`,
		Members: []Member{
			{ID: "a", ContentHash: "ha"},
			{ID: "b", ContentHash: "hb"},
		},
	})
	if err == nil {
		t.Fatal("mismatched direct candidate key should fail")
	}
}

func (m mapChecker) MaintenanceCheckCompleted(_ context.Context, _ string, _ SweepType, candidate Candidate) (bool, error) {
	return m.done[candidate.Key], nil
}

func mustCandidate(t *testing.T, c Candidate, err error) Candidate {
	t.Helper()
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	return c
}

func mustPair(t *testing.T, a, b Member) Candidate {
	t.Helper()
	c, err := Pair(a, b)
	return mustCandidate(t, c, err)
}

func mustCluster(t *testing.T, members ...Member) Candidate {
	t.Helper()
	c, err := Cluster(members...)
	return mustCandidate(t, c, err)
}
