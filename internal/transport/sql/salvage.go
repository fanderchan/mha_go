package sql

import (
	"context"
	sqlstd "database/sql"
	"fmt"
	"strings"
	"time"

	"mha-go/internal/domain"
)

// WaitForExecutedGTIDSet blocks until the replica has applied all GTIDs in the set (or timeout).
func WaitForExecutedGTIDSet(ctx context.Context, db *sqlstd.DB, gtidSet string, timeout time.Duration) error {
	gtidSet = strings.TrimSpace(gtidSet)
	if gtidSet == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	sec := timeout.Seconds()
	var rc sqlstd.NullInt64
	err := db.QueryRowContext(ctx, `SELECT WAIT_FOR_EXECUTED_GTID_SET(?, ?)`, gtidSet, sec).Scan(&rc)
	if err != nil {
		return fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET: %w", err)
	}
	if !rc.Valid {
		return fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET returned NULL")
	}
	switch rc.Int64 {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET timed out after %s", timeout)
	default:
		return fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET returned status %d", rc.Int64)
	}
}

// SalvageCatchUpFromDonor points the target at the donor with GTID auto-position, starts replication,
// then waits until the missing GTID subset is executed on the target.
func SalvageCatchUpFromDonor(ctx context.Context, db *sqlstd.DB, donor domain.NodeSpec, donorPassword string, missingGTID string, wait time.Duration) error {
	if err := RepointReplicaToSource(ctx, db, donor, donorPassword); err != nil {
		return fmt.Errorf("repoint target at donor %q: %w", donor.ID, err)
	}
	return WaitForExecutedGTIDSet(ctx, db, missingGTID, wait)
}
