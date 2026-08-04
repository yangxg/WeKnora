package querycursor

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	policyA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	policyB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	lane    = "Academic"
)

func TestDecodeReturnsNilWhenThereIsNothingToResumeFrom(t *testing.T) {
	ctx := context.Background()
	if got := Decode(ctx, nil, policyA, lane); got != nil {
		t.Errorf("a nil sync cursor decoded to %+v", got)
	}
	if got := Decode(ctx, &types.SyncCursor{LastSchemaHash: policyA}, policyA, lane); got != nil {
		t.Errorf("a sync cursor with no connector state decoded to %+v", got)
	}
}

// A cursor built under a different policy is discarded rather than trusted. After
// a policy edit, candidates the old policy never looked at would otherwise stay
// invisible — and a stale cursor is invisible until someone notices what is
// missing, which is the worst shape a bug can have here.
func TestDecodeDiscardsStateFromADifferentPolicy(t *testing.T) {
	stored := New()
	stored.Record("q1", "q1:doi:10.1/a", "h:cafe")

	persisted := stored.ToSyncCursor(context.Background(), policyA, lane)
	if got := Decode(context.Background(), persisted, policyB, lane); got != nil {
		t.Fatalf("state from policy A survived a switch to policy B: %+v", got)
	}
	if got := Decode(context.Background(), persisted, policyA, lane); got == nil {
		t.Fatal("state from the same policy was discarded")
	}
}

func TestRoundTripPreservesProgress(t *testing.T) {
	ctx := context.Background()
	stored := New()
	stored.StartQuery("q1")
	stored.Record("q1", "q1:doi:10.1/a", "h:aaaa")
	stored.Record("q1", "q1:doi:10.1/b", "h:bbbb")
	stored.StartQuery("q2")

	persisted := stored.ToSyncCursor(ctx, policyA, lane)
	if persisted.LastSchemaHash != policyA {
		t.Errorf("LastSchemaHash = %q, want the policy digest", persisted.LastSchemaHash)
	}

	restored := Decode(ctx, persisted, policyA, lane)
	if restored == nil {
		t.Fatal("Decode returned nil for its own output")
	}
	if got := PriorProgress(restored, "q1"); len(got) != 2 || got["q1:doi:10.1/a"] != "h:aaaa" {
		t.Errorf("q1 progress = %v", got)
	}
	// An empty slot is not the same as an absent one: it says the query ran and
	// found nothing new, rather than that it never ran.
	if _, ok := restored.Queries["q2"]; !ok {
		t.Error("a query that ran and found nothing lost its slot")
	}
}

func TestDecodeRefusesAnUnreadableBlob(t *testing.T) {
	// A connector cursor whose "queries" is a string cannot be the shape this
	// package writes, so it is discarded rather than partially believed.
	corrupt := &types.SyncCursor{
		LastSchemaHash:  policyA,
		ConnectorCursor: map[string]interface{}{"queries": "not-a-map"},
	}
	if got := Decode(context.Background(), corrupt, policyA, lane); got != nil {
		t.Errorf("a corrupt cursor decoded to %+v", got)
	}
}

// The distinction this test protects is the whole reason the package exists: a
// query whose *search* failed keeps what it had, while a *candidate* that failed
// is simply never recorded and so gets retried.
func TestCarryQueryProgressKeepsAFailedSearchFromReEmitting(t *testing.T) {
	prev := New()
	prev.Record("q1", "q1:doi:10.1/a", "h:aaaa")
	prev.Record("q2", "q2:doi:10.1/z", "h:zzzz")

	next := New()
	// q1's search failed this run.
	next.CarryQueryProgress(prev, "q1")
	// q2 ran; one of its candidates failed to render and was not recorded.
	next.StartQuery("q2")

	if got := PriorProgress(next, "q1"); len(got) != 1 || got["q1:doi:10.1/a"] != "h:aaaa" {
		t.Errorf("q1 progress = %v, want it carried forward", got)
	}
	if got := PriorProgress(next, "q2"); len(got) != 0 {
		t.Errorf("q2 progress = %v, want the failed candidate left unrecorded so it retries", got)
	}
}

// Carrying must copy, not alias: this run's Record calls would otherwise mutate
// the state the caller may still be comparing against.
func TestCarryQueryProgressCopiesRatherThanAliases(t *testing.T) {
	prev := New()
	prev.Record("q1", "q1:doi:10.1/a", "h:aaaa")

	next := New()
	next.CarryQueryProgress(prev, "q1")
	next.Record("q1", "q1:doi:10.1/new", "h:nnnn")

	if _, leaked := prev.Queries["q1"]["q1:doi:10.1/new"]; leaked {
		t.Error("recording into the new cursor mutated the prior one")
	}
}

func TestCarryQueryProgressIsANoOpWhenThereIsNothingToCarry(t *testing.T) {
	next := New()
	next.CarryQueryProgress(nil, "q1")
	next.CarryQueryProgress(New(), "q1")
	if len(next.Queries) != 0 {
		t.Errorf("Queries = %v, want no slot invented for a query with no prior state", next.Queries)
	}

	var nilCursor *Cursor
	nilCursor.CarryQueryProgress(New(), "q1")
	nilCursor.Record("q1", "x", "h:1")
	nilCursor.StartQuery("q1")
	if got := PriorProgress(nil, "q1"); got != nil {
		t.Errorf("PriorProgress(nil) = %v", got)
	}
}

func TestFingerprintIsStableAndDiscriminating(t *testing.T) {
	const card = "# A work\n\nDOI: 10.1/a\n"
	first, second := Fingerprint(card), Fingerprint(card)
	if first != second {
		t.Fatalf("Fingerprint is not stable: %q vs %q", first, second)
	}
	if first == Fingerprint(card+" ") {
		t.Error("a changed candidate kept its fingerprint and would be skipped forever")
	}
	if Fingerprint("") == "" {
		t.Error("empty content still needs a fingerprint, or it would compare equal to a missing slot")
	}
}
