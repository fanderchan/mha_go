package sql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/replication"
)

// SQLSalvager implements replication.Salvager using direct SQL connections.
//
// CollectMissingTransactions verifies that a donor node is reachable via SQL and
// holds the required GTID set, then returns an artifact encoding the donor and GTIDs.
// ApplyTransactions points the candidate at the donor and waits for catch-up.
//
// This implementation covers the primary salvage path (blueprint §7.3 priority 1):
// old primary or a surviving replica is accessible over SQL.
type SQLSalvager struct {
	inspector *MySQLInspector
}

func NewSQLSalvager(inspector *MySQLInspector) *SQLSalvager {
	return &SQLSalvager{inspector: inspector}
}

// CollectMissingTransactions finds the best available donor for the missing GTID set.
// It tries the old primary first, then surviving replicas, and returns the first donor
// that is reachable and holds the required transactions.
func (s *SQLSalvager) CollectMissingTransactions(
	ctx context.Context,
	spec domain.ClusterSpec,
	view *domain.ClusterView,
	oldPrimary, candidate domain.NodeState,
	missingGTIDSet string,
) (replication.SalvageArtifact, error) {
	if strings.TrimSpace(missingGTIDSet) == "" {
		return replication.SalvageArtifact{}, nil
	}

	// Try old primary first (blueprint §7.3 priority 1).
	if oldPrimary.Health != domain.NodeHealthDead {
		if ns, ok := nodeSpecByID(spec, oldPrimary.ID); ok {
			if err := s.verifyDonorHasGTIDs(ctx, ns, missingGTIDSet); err == nil {
				return encodeSalvageArtifact(oldPrimary.ID, missingGTIDSet), nil
			}
		}
	}

	// Fall back to surviving replicas that hold the missing set.
	for _, node := range view.Nodes {
		if node.ID == candidate.ID || node.ID == oldPrimary.ID {
			continue
		}
		if node.Health == domain.NodeHealthDead || node.Role != domain.NodeRoleReplica {
			continue
		}
		ns, ok := nodeSpecByID(spec, node.ID)
		if !ok {
			continue
		}
		if err := s.verifyDonorHasGTIDs(ctx, ns, missingGTIDSet); err == nil {
			return encodeSalvageArtifact(node.ID, missingGTIDSet), nil
		}
	}

	return replication.SalvageArtifact{}, fmt.Errorf(
		"no SQL-accessible donor found that holds missing GTID set %q", missingGTIDSet)
}

// ApplyTransactions points the candidate at the donor encoded in the artifact and
// waits until it has applied all the missing GTIDs.
func (s *SQLSalvager) ApplyTransactions(
	ctx context.Context,
	spec domain.ClusterSpec,
	candidate domain.NodeState,
	artifact replication.SalvageArtifact,
) error {
	donorID, missingGTID, err := decodeSalvageArtifact(artifact)
	if err != nil {
		return fmt.Errorf("decode salvage artifact: %w", err)
	}
	if missingGTID == "" {
		return nil
	}

	donorSpec, ok := nodeSpecByID(spec, donorID)
	if !ok {
		return fmt.Errorf("donor node %q not found in cluster spec", donorID)
	}
	candSpec, ok := nodeSpecByID(spec, candidate.ID)
	if !ok {
		return fmt.Errorf("candidate node %q not found in cluster spec", candidate.ID)
	}

	donorPassword, err := s.inspector.ResolvePassword(ctx, donorSpec.SQL.PasswordRef)
	if err != nil {
		return fmt.Errorf("resolve donor %q password: %w", donorID, err)
	}

	timeout := spec.Replication.Salvage.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	candDB, err := s.inspector.OpenDB(ctx, candSpec)
	if err != nil {
		return fmt.Errorf("connect to candidate %q for salvage: %w", candidate.ID, err)
	}
	defer candDB.Close()

	return SalvageCatchUpFromDonor(ctx, candDB, donorSpec, donorPassword, missingGTID, timeout)
}

// verifyDonorHasGTIDs opens a SQL connection to the donor and checks that its
// gtid_executed contains the required GTID set.
func (s *SQLSalvager) verifyDonorHasGTIDs(ctx context.Context, donorSpec domain.NodeSpec, requiredGTID string) error {
	db, err := s.inspector.OpenDB(ctx, donorSpec)
	if err != nil {
		return err
	}
	defer db.Close()

	donorGTID, err := GetGTIDExecuted(ctx, db)
	if err != nil {
		return err
	}

	has, _, err := containsGTID(donorGTID, requiredGTID)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("donor %s gtid_executed does not contain required set", donorSpec.ID)
	}
	return nil
}

// encodeSalvageArtifact packs donorID and missingGTID into a SalvageArtifact.
// Format: "<donorID>\x00<missingGTID>"
func encodeSalvageArtifact(donorID, missingGTID string) replication.SalvageArtifact {
	return replication.SalvageArtifact{Reference: donorID + "\x00" + missingGTID}
}

func decodeSalvageArtifact(a replication.SalvageArtifact) (donorID, missingGTID string, err error) {
	parts := strings.SplitN(a.Reference, "\x00", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("malformed salvage artifact reference %q", a.Reference)
	}
	return parts[0], parts[1], nil
}

// containsGTID returns (true, true, nil) when supersetRaw contains subsetRaw.
func containsGTID(supersetRaw, subsetRaw string) (contains bool, known bool, err error) {
	if strings.TrimSpace(subsetRaw) == "" {
		return true, true, nil
	}
	// Reuse the SubtractGTIDSets helper: if superset-subset == empty, superset contains subset.
	diff, known, err := subtractGTIDSets(supersetRaw, subsetRaw)
	if err != nil || !known {
		return false, known, err
	}
	return strings.TrimSpace(diff) == "", true, nil
}

// subtractGTIDSets is a thin wrapper around the raw SQL string approach used elsewhere.
// It avoids importing the replication package from transport/sql.
func subtractGTIDSets(leftRaw, rightRaw string) (string, bool, error) {
	left, err := parseGTIDSetLocal(leftRaw)
	if err != nil {
		return "", false, err
	}
	right, err := parseGTIDSetLocal(rightRaw)
	if err != nil {
		return "", false, err
	}
	result := subtractLocalGTID(left, right)
	return result, true, nil
}

// ---- minimal local GTID set helpers to avoid importing the replication package ----

type localGTIDSet map[string][]localInterval

type localInterval struct{ start, end uint64 }

func parseGTIDSetLocal(raw string) (localGTIDSet, error) {
	out := make(localGTIDSet)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		parts := strings.SplitN(token, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid GTID token %q", token)
		}
		sid := strings.ToLower(strings.TrimSpace(parts[0]))
		for _, rawInterval := range strings.Split(parts[1], ":") {
			iv, err := parseLocalInterval(rawInterval)
			if err != nil {
				return nil, err
			}
			out[sid] = append(out[sid], iv)
		}
	}
	return out, nil
}

func parseLocalInterval(raw string) (localInterval, error) {
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "-"); idx >= 0 {
		var s, e uint64
		if _, err := fmt.Sscanf(raw[:idx], "%d", &s); err != nil {
			return localInterval{}, fmt.Errorf("bad interval start %q", raw)
		}
		if _, err := fmt.Sscanf(raw[idx+1:], "%d", &e); err != nil {
			return localInterval{}, fmt.Errorf("bad interval end %q", raw)
		}
		return localInterval{s, e}, nil
	}
	var n uint64
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return localInterval{}, fmt.Errorf("bad interval %q", raw)
	}
	return localInterval{n, n}, nil
}

func subtractLocalGTID(left, right localGTIDSet) string {
	result := make(localGTIDSet)
	for sid, intervals := range left {
		cur := intervals
		for _, rm := range right[sid] {
			cur = subtractLocalIntervals(cur, rm)
		}
		if len(cur) > 0 {
			result[sid] = cur
		}
	}
	return localGTIDSetString(result)
}

func subtractLocalIntervals(intervals []localInterval, rm localInterval) []localInterval {
	out := make([]localInterval, 0, len(intervals))
	for _, iv := range intervals {
		if rm.end < iv.start || rm.start > iv.end {
			out = append(out, iv)
			continue
		}
		if rm.start <= iv.start && rm.end >= iv.end {
			continue
		}
		if rm.start > iv.start {
			out = append(out, localInterval{iv.start, rm.start - 1})
		}
		if rm.end < iv.end {
			out = append(out, localInterval{rm.end + 1, iv.end})
		}
	}
	return out
}

func localGTIDSetString(s localGTIDSet) string {
	if len(s) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s))
	for sid, intervals := range s {
		for _, iv := range intervals {
			if iv.start == iv.end {
				parts = append(parts, fmt.Sprintf("%s:%d", sid, iv.start))
			} else {
				parts = append(parts, fmt.Sprintf("%s:%d-%d", sid, iv.start, iv.end))
			}
		}
	}
	return strings.Join(parts, ",")
}

func nodeSpecByID(spec domain.ClusterSpec, id string) (domain.NodeSpec, bool) {
	for _, n := range spec.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return domain.NodeSpec{}, false
}
