package replication

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mha-go/internal/domain"
)

type Interval struct {
	Start uint64
	End   uint64
}

type GTIDSet map[string][]Interval

type RecoverySummary struct {
	CandidateFreshnessScore int
	CandidateMostAdvanced   bool
	MissingFromPrimaryKnown bool
	MissingFromPrimarySet   string
	RecoveryGaps            []domain.RecoveryGap
	SalvageActions          []domain.SalvageAction
	SuggestedDonorIDs       []string
}

func ParseGTIDSet(raw string) (GTIDSet, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return GTIDSet{}, nil
	}

	out := make(GTIDSet)
	for _, token := range strings.Split(value, ",") {
		item := strings.TrimSpace(token)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("invalid GTID set token %q", item)
		}
		sid := strings.ToLower(strings.TrimSpace(parts[0]))
		if sid == "" {
			return nil, fmt.Errorf("invalid GTID set token %q: empty SID", item)
		}
		intervals := out[sid]
		for _, rawInterval := range parts[1:] {
			interval, err := parseInterval(rawInterval)
			if err != nil {
				return nil, fmt.Errorf("invalid GTID set token %q: %w", item, err)
			}
			intervals = append(intervals, interval)
		}
		out[sid] = normalizeIntervals(intervals)
	}
	return out, nil
}

func (s GTIDSet) String() string {
	if len(s) == 0 {
		return ""
	}
	sids := make([]string, 0, len(s))
	for sid := range s {
		sids = append(sids, sid)
	}
	sort.Strings(sids)

	parts := make([]string, 0, len(sids))
	for _, sid := range sids {
		intervals := normalizeIntervals(s[sid])
		if len(intervals) == 0 {
			continue
		}
		encoded := make([]string, 0, len(intervals)+1)
		encoded = append(encoded, sid)
		for _, interval := range intervals {
			if interval.Start == interval.End {
				encoded = append(encoded, strconv.FormatUint(interval.Start, 10))
			} else {
				encoded = append(encoded, fmt.Sprintf("%d-%d", interval.Start, interval.End))
			}
		}
		parts = append(parts, strings.Join(encoded, ":"))
	}
	return strings.Join(parts, ",")
}

func (s GTIDSet) IsEmpty() bool {
	for _, intervals := range s {
		if len(intervals) > 0 {
			return false
		}
	}
	return true
}

func SubtractGTIDSets(leftRaw, rightRaw string) (string, bool, error) {
	left, err := ParseGTIDSet(leftRaw)
	if err != nil {
		return "", false, err
	}
	right, err := ParseGTIDSet(rightRaw)
	if err != nil {
		return "", false, err
	}
	return left.Subtract(right).String(), true, nil
}

func ContainsGTIDSet(supersetRaw, subsetRaw string) (bool, bool, error) {
	superset, err := ParseGTIDSet(supersetRaw)
	if err != nil {
		return false, false, err
	}
	subset, err := ParseGTIDSet(subsetRaw)
	if err != nil {
		return false, false, err
	}
	return superset.Contains(subset), true, nil
}

func (s GTIDSet) Contains(other GTIDSet) bool {
	for sid, otherIntervals := range other {
		if !containsIntervals(normalizeIntervals(s[sid]), normalizeIntervals(otherIntervals)) {
			return false
		}
	}
	return true
}

func (s GTIDSet) Subtract(other GTIDSet) GTIDSet {
	if len(s) == 0 {
		return GTIDSet{}
	}
	out := make(GTIDSet, len(s))
	for sid, intervals := range s {
		current := normalizeIntervals(intervals)
		for _, otherInterval := range normalizeIntervals(other[sid]) {
			current = subtractIntervalSet(current, otherInterval)
			if len(current) == 0 {
				break
			}
		}
		if len(current) > 0 {
			out[sid] = current
		}
	}
	return out
}

func CandidateFreshnessScore(candidate domain.NodeState, nodes []domain.NodeState) int {
	candidateSet, err := ParseGTIDSet(candidate.GTIDExecuted)
	if err != nil || candidateSet.IsEmpty() {
		return 0
	}

	score := 0
	for _, node := range nodes {
		if node.ID == candidate.ID || node.Role != domain.NodeRoleReplica || node.Health == domain.NodeHealthDead {
			continue
		}
		otherSet, err := ParseGTIDSet(node.GTIDExecuted)
		if err != nil || otherSet.IsEmpty() {
			continue
		}
		switch {
		case candidateSet.Contains(otherSet):
			score += 25
		case otherSet.Contains(candidateSet):
			score -= 25
		default:
			score += 5
		}
	}
	return score
}

func BuildRecoverySummary(view *domain.ClusterView, oldPrimary, candidate domain.NodeState, policy domain.SalvagePolicy) RecoverySummary {
	summary := RecoverySummary{
		CandidateFreshnessScore: CandidateFreshnessScore(candidate, view.Nodes),
		CandidateMostAdvanced:   true,
	}

	if strings.TrimSpace(oldPrimary.GTIDExecuted) != "" {
		if missing, known, err := SubtractGTIDSets(oldPrimary.GTIDExecuted, candidate.GTIDExecuted); err == nil {
			summary.MissingFromPrimaryKnown = known
			summary.MissingFromPrimarySet = missing
		}
	}

	donorSet := make(map[string]struct{})
	for _, node := range view.Nodes {
		if node.ID == candidate.ID || node.Role != domain.NodeRoleReplica || node.Health == domain.NodeHealthDead {
			continue
		}
		if strings.TrimSpace(node.GTIDExecuted) == "" {
			continue
		}
		missing, known, err := SubtractGTIDSets(node.GTIDExecuted, candidate.GTIDExecuted)
		if err != nil {
			continue
		}
		if known && missing != "" {
			summary.CandidateMostAdvanced = false
			summary.RecoveryGaps = append(summary.RecoveryGaps, domain.RecoveryGap{
				SourceNodeID:     node.ID,
				MissingGTIDSet:   missing,
				MissingGTIDKnown: true,
			})
			donorSet[node.ID] = struct{}{}
		}
	}

	if summary.MissingFromPrimaryKnown && summary.MissingFromPrimarySet != "" {
		summary.CandidateMostAdvanced = false
		if oldPrimary.ID != "" {
			donorSet[oldPrimary.ID] = struct{}{}
		}
	}

	for donorID := range donorSet {
		summary.SuggestedDonorIDs = append(summary.SuggestedDonorIDs, donorID)
	}
	sort.Strings(summary.SuggestedDonorIDs)
	summary.SalvageActions = BuildSalvageActions(oldPrimary, candidate, summary, policy)
	return summary
}

func BuildSalvageActions(oldPrimary, candidate domain.NodeState, summary RecoverySummary, policy domain.SalvagePolicy) []domain.SalvageAction {
	// availability-first: salvage is best-effort; failure warns but does not abort.
	required := policy != domain.SalvageAvailabilityFirst
	actions := make([]domain.SalvageAction, 0, len(summary.RecoveryGaps)+1)
	if summary.MissingFromPrimaryKnown && summary.MissingFromPrimarySet != "" {
		actions = append(actions, domain.SalvageAction{
			Kind:           "recover-from-old-primary",
			DonorNodeID:    oldPrimary.ID,
			TargetNodeID:   candidate.ID,
			MissingGTIDSet: summary.MissingFromPrimarySet,
			Required:       required,
			Reason:         "candidate is missing GTIDs that only the old primary is known to have executed",
		})
	}
	for _, gap := range summary.RecoveryGaps {
		if !gap.MissingGTIDKnown || gap.MissingGTIDSet == "" {
			continue
		}
		actions = append(actions, domain.SalvageAction{
			Kind:           "recover-from-replica",
			DonorNodeID:    gap.SourceNodeID,
			TargetNodeID:   candidate.ID,
			MissingGTIDSet: gap.MissingGTIDSet,
			Required:       required,
			Reason:         "candidate is behind another surviving replica",
		})
	}
	return actions
}

func parseInterval(raw string) (Interval, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Interval{}, fmt.Errorf("empty interval")
	}
	if !strings.Contains(value, "-") {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return Interval{}, fmt.Errorf("invalid interval %q", raw)
		}
		return Interval{Start: n, End: n}, nil
	}
	parts := strings.SplitN(value, "-", 2)
	start, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return Interval{}, fmt.Errorf("invalid interval start %q", raw)
	}
	end, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return Interval{}, fmt.Errorf("invalid interval end %q", raw)
	}
	if end < start {
		return Interval{}, fmt.Errorf("interval end %d is smaller than start %d", end, start)
	}
	return Interval{Start: start, End: end}, nil
}

func normalizeIntervals(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	out := append([]Interval(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End < out[j].End
	})

	merged := out[:1]
	for _, interval := range out[1:] {
		last := &merged[len(merged)-1]
		if interval.Start <= last.End+1 {
			if interval.End > last.End {
				last.End = interval.End
			}
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

func containsIntervals(have, want []Interval) bool {
	if len(want) == 0 {
		return true
	}
	if len(have) == 0 {
		return false
	}
	i := 0
	for _, target := range want {
		for i < len(have) && have[i].End < target.Start {
			i++
		}
		if i == len(have) {
			return false
		}
		if have[i].Start > target.Start || have[i].End < target.End {
			return false
		}
	}
	return true
}

func subtractIntervalSet(intervals []Interval, remove Interval) []Interval {
	if len(intervals) == 0 {
		return nil
	}
	out := make([]Interval, 0, len(intervals))
	for _, interval := range intervals {
		switch {
		case remove.End < interval.Start || remove.Start > interval.End:
			out = append(out, interval)
		case remove.Start <= interval.Start && remove.End >= interval.End:
			continue
		case remove.Start > interval.Start && remove.End < interval.End:
			out = append(out, Interval{Start: interval.Start, End: remove.Start - 1})
			out = append(out, Interval{Start: remove.End + 1, End: interval.End})
		case remove.Start <= interval.Start:
			out = append(out, Interval{Start: remove.End + 1, End: interval.End})
		default:
			out = append(out, Interval{Start: interval.Start, End: remove.Start - 1})
		}
	}
	return out
}
