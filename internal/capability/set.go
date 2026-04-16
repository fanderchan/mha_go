package capability

import (
	"fmt"
	"strings"

	"mha-go/internal/domain"
)

const (
	VersionSeries84 = "8.4"
	VersionSeries97 = "9.7"
)

func NormalizeVersionSeries(raw string) (string, error) {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch {
	case value == "":
		return "", fmt.Errorf("must be set")
	case strings.HasPrefix(value, "8.4"):
		return VersionSeries84, nil
	case strings.HasPrefix(value, "9.7"):
		return VersionSeries97, nil
	default:
		return "", fmt.Errorf("unsupported version series %q; only 8.4 and 9.7 are allowed", raw)
	}
}

func BaselineForVersionSeries(series string) (domain.CapabilitySet, error) {
	normalized, err := NormalizeVersionSeries(series)
	if err != nil {
		return domain.CapabilitySet{}, err
	}

	switch normalized {
	case VersionSeries84, VersionSeries97:
		return domain.CapabilitySet{
			HasGTID:                     true,
			HasAutoPosition:             true,
			HasSuperReadOnly:            true,
			HasSemiSync:                 true,
			HasPerfSchemaReplication:    true,
			HasClonePlugin:              true,
			SupportsReplicationChannels: true,
			SupportsDynamicPrivileges:   true,
			SupportsReadOnlyFence:       true,
		}, nil
	default:
		return domain.CapabilitySet{}, fmt.Errorf("unsupported normalized version %q", normalized)
	}
}
