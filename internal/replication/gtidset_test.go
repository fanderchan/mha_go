package replication

import (
	"testing"

	"mha-go/internal/domain"
)

func TestParseSubtractAndContainsGTIDSet(t *testing.T) {
	raw := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-5:7-9,bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb:1-3"
	set, err := ParseGTIDSet(raw)
	if err != nil {
		t.Fatalf("parse GTID set: %v", err)
	}
	if got := set.String(); got != raw {
		t.Fatalf("round trip = %q, want %q", got, raw)
	}

	diff, known, err := SubtractGTIDSets(
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-5:7-9",
	)
	if err != nil {
		t.Fatalf("subtract GTID sets: %v", err)
	}
	if !known {
		t.Fatal("subtract result should be known")
	}
	if want := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:6:10"; diff != want {
		t.Fatalf("subtract result = %q, want %q", diff, want)
	}

	contains, known, err := ContainsGTIDSet(
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:2-3:8-9",
	)
	if err != nil {
		t.Fatalf("contains GTID set: %v", err)
	}
	if !known || !contains {
		t.Fatalf("contains = %t known = %t, want true true", contains, known)
	}
}

func TestCandidateFreshnessAndRecoverySummary(t *testing.T) {
	view := &domain.ClusterView{
		PrimaryID: "db1",
		Nodes: []domain.NodeState{
			{
				ID:           "db1",
				Role:         domain.NodeRolePrimary,
				GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-15",
			},
			{
				ID:           "db2",
				Role:         domain.NodeRoleReplica,
				Health:       domain.NodeHealthAlive,
				GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
			},
			{
				ID:           "db3",
				Role:         domain.NodeRoleReplica,
				Health:       domain.NodeHealthAlive,
				GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-12",
			},
		},
	}

	score := CandidateFreshnessScore(view.Nodes[2], view.Nodes)
	if score <= 0 {
		t.Fatalf("freshness score = %d, want positive", score)
	}

	summary := BuildRecoverySummary(view, view.Nodes[0], view.Nodes[2], domain.SalvageIfPossible)
	if summary.CandidateMostAdvanced {
		t.Fatal("candidate should not be most advanced because primary is ahead")
	}
	if !summary.MissingFromPrimaryKnown {
		t.Fatal("missing from primary should be known")
	}
	if summary.MissingFromPrimarySet != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:13-15" {
		t.Fatalf("missing from primary = %q", summary.MissingFromPrimarySet)
	}
	if len(summary.SuggestedDonorIDs) != 1 || summary.SuggestedDonorIDs[0] != "db1" {
		t.Fatalf("suggested donors = %+v, want [db1]", summary.SuggestedDonorIDs)
	}
	if len(summary.SalvageActions) != 1 {
		t.Fatalf("salvage actions = %+v, want 1 action", summary.SalvageActions)
	}
	if summary.SalvageActions[0].Kind != "recover-from-old-primary" {
		t.Fatalf("salvage action kind = %s, want recover-from-old-primary", summary.SalvageActions[0].Kind)
	}
}
