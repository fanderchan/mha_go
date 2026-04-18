package sql

import (
	"context"
	sqlstd "database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mha-go/internal/domain"
)

type ReadOnlyFenceDegradedError struct {
	SuperReadOnlyErr error
}

func (e *ReadOnlyFenceDegradedError) Error() string {
	return fmt.Sprintf("super_read_only fence failed; read_only fallback applied: %v", e.SuperReadOnlyErr)
}

func (e *ReadOnlyFenceDegradedError) Unwrap() error {
	return e.SuperReadOnlyErr
}

func IsReadOnlyFenceDegraded(err error) bool {
	var degraded *ReadOnlyFenceDegradedError
	return errors.As(err, &degraded)
}

// GetGTIDExecuted returns the current @@GLOBAL.gtid_executed set on the instance.
func GetGTIDExecuted(ctx context.Context, db *sqlstd.DB) (string, error) {
	var gtid string
	if err := db.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_executed`).Scan(&gtid); err != nil {
		return "", fmt.Errorf("get gtid_executed: %w", err)
	}
	return strings.TrimSpace(gtid), nil
}

// WaitForStableGTIDAfterReadOnly waits until pre-existing write transactions drain
// after the old primary has been fenced read-only, then returns a stable GTID snapshot.
//
// Read-only mode rejects new writes, but it does not wait for transactions that
// already executed DML before the fence. Without this drain + stable snapshot, a
// late COMMIT can create a GTID after mha-go has already told the candidate what
// to wait for, which risks promoting a node that is missing that transaction.
func WaitForStableGTIDAfterReadOnly(ctx context.Context, db *sqlstd.DB, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const stableSamples = 3
	const pollInterval = 200 * time.Millisecond

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastGTID string
	haveLastGTID := false
	stableCount := 0
	for {
		activeWrites, err := countActiveInnoDBWriteTransactions(waitCtx, db)
		if err != nil {
			return "", err
		}
		gtid, err := GetGTIDExecuted(waitCtx, db)
		if err != nil {
			return "", err
		}

		if activeWrites == 0 {
			if haveLastGTID && gtid == lastGTID {
				stableCount++
			} else {
				lastGTID = gtid
				haveLastGTID = true
				stableCount = 1
			}
			if stableCount >= stableSamples {
				return gtid, nil
			}
		} else {
			lastGTID = gtid
			haveLastGTID = true
			stableCount = 0
		}

		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("wait for old primary GTID quiescence after read-only fence: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func countActiveInnoDBWriteTransactions(ctx context.Context, db *sqlstd.DB) (int, error) {
	var active int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM information_schema.innodb_trx
WHERE trx_mysql_thread_id <> CONNECTION_ID()
  AND trx_rows_modified > 0`).Scan(&active)
	if err != nil {
		return 0, fmt.Errorf("inspect active InnoDB write transactions: %w", err)
	}
	return active, nil
}

type ReadOnlyState struct {
	ReadOnly      bool
	SuperReadOnly bool
}

func GetReadOnlyState(ctx context.Context, db *sqlstd.DB) (ReadOnlyState, error) {
	var readOnlyRaw, superReadOnlyRaw string
	if err := db.QueryRowContext(ctx, `SELECT @@GLOBAL.read_only, @@GLOBAL.super_read_only`).Scan(&readOnlyRaw, &superReadOnlyRaw); err != nil {
		return ReadOnlyState{}, fmt.Errorf("get read-only state: %w", err)
	}
	return ReadOnlyState{
		ReadOnly:      parseBoolish(readOnlyRaw),
		SuperReadOnly: parseBoolish(superReadOnlyRaw),
	}, nil
}

func TemporarilyDisableReadOnly(ctx context.Context, db *sqlstd.DB) (func(context.Context) error, error) {
	original, err := GetReadOnlyState(ctx, db)
	if err != nil {
		return nil, err
	}
	if original.SuperReadOnly {
		if _, err := db.ExecContext(ctx, `SET GLOBAL super_read_only = OFF`); err != nil {
			return nil, fmt.Errorf("set super_read_only off: %w", err)
		}
	}
	if original.ReadOnly {
		if _, err := db.ExecContext(ctx, `SET GLOBAL read_only = OFF`); err != nil {
			// Roll back the earlier super_read_only=OFF so the caller is not left with
			// a weaker state than they started with when no restore closure can be returned.
			if original.SuperReadOnly {
				_, _ = db.ExecContext(ctx, `SET GLOBAL super_read_only = ON`)
			}
			return nil, fmt.Errorf("set read_only off: %w", err)
		}
	}
	return func(ctx context.Context) error {
		if original.ReadOnly {
			if _, err := db.ExecContext(ctx, `SET GLOBAL read_only = ON`); err != nil {
				return fmt.Errorf("restore read_only: %w", err)
			}
		}
		if original.SuperReadOnly {
			if _, err := db.ExecContext(ctx, `SET GLOBAL super_read_only = ON`); err != nil {
				return fmt.Errorf("restore super_read_only: %w", err)
			}
		}
		return nil
	}, nil
}

// FenceReadOnly enables super_read_only/read_only on the instance to block writes (SQL-side fencing).
func FenceReadOnly(ctx context.Context, db *sqlstd.DB, caps domain.CapabilitySet) error {
	if caps.SupportsReadOnlyFence && caps.HasSuperReadOnly {
		if _, err := db.ExecContext(ctx, `SET GLOBAL super_read_only = ON`); err == nil {
			return nil
		} else {
			if _, roErr := db.ExecContext(ctx, `SET GLOBAL read_only = ON`); roErr != nil {
				return fmt.Errorf("set super_read_only: %w; set read_only fallback: %v", err, roErr)
			}
			return &ReadOnlyFenceDegradedError{SuperReadOnlyErr: err}
		}
	}
	if _, err := db.ExecContext(ctx, `SET GLOBAL read_only = ON`); err != nil {
		return fmt.Errorf("set read_only: %w", err)
	}
	return nil
}

// StopAndResetReplica quiesces a candidate's replica channel with STOP REPLICA + RESET REPLICA ALL.
// Intended as a cleanup after a failed salvage attempt so later recovery paths do not race with
// a still-running IO/SQL thread pulling from the same donor.
func StopAndResetReplica(ctx context.Context, db *sqlstd.DB) error {
	if _, err := db.ExecContext(ctx, `STOP REPLICA`); err != nil {
		return fmt.Errorf("stop replica: %w", err)
	}
	if _, err := db.ExecContext(ctx, `RESET REPLICA ALL`); err != nil {
		return fmt.Errorf("reset replica all: %w", err)
	}
	return nil
}

// PromoteReplicaToPrimary stops replication, clears replica metadata, and enables writes on the new primary.
func PromoteReplicaToPrimary(ctx context.Context, db *sqlstd.DB) error {
	if _, err := db.ExecContext(ctx, `STOP REPLICA`); err != nil {
		return fmt.Errorf("stop replica: %w", err)
	}
	if _, err := db.ExecContext(ctx, `RESET REPLICA ALL`); err != nil {
		return fmt.Errorf("reset replica all: %w", err)
	}
	if _, err := db.ExecContext(ctx, `SET GLOBAL super_read_only = OFF`); err != nil {
		return fmt.Errorf("set super_read_only off: %w", err)
	}
	if _, err := db.ExecContext(ctx, `SET GLOBAL read_only = OFF`); err != nil {
		return fmt.Errorf("set read_only off: %w", err)
	}
	return nil
}

// RepointReplicaToSource points a replica at the new primary using GTID auto-position.
// sourcePassword must be the replication account password accepted on the source (candidate) instance.
func RepointReplicaToSource(ctx context.Context, db *sqlstd.DB, source domain.NodeSpec, sourcePassword string) error {
	if _, err := db.ExecContext(ctx, `STOP REPLICA`); err != nil {
		return fmt.Errorf("stop replica: %w", err)
	}
	q := buildChangeReplicationSourceSQL(source, sourcePassword)
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("change replication source: %w", err)
	}
	if _, err := db.ExecContext(ctx, `START REPLICA`); err != nil {
		return fmt.Errorf("start replica: %w", err)
	}
	return nil
}

func buildChangeReplicationSourceSQL(source domain.NodeSpec, sourcePassword string) string {
	host := escapeSQLString(source.Host)
	user := escapeSQLString(source.SQL.ReplicationUserOrDefault())
	pass := escapeSQLString(sourcePassword)
	var b strings.Builder
	b.WriteString(`CHANGE REPLICATION SOURCE TO `)
	b.WriteString(fmt.Sprintf(`SOURCE_HOST='%s', SOURCE_PORT=%d, SOURCE_USER='%s', SOURCE_PASSWORD='%s', SOURCE_AUTO_POSITION=1`,
		host, source.Port, user, pass))
	if tls := replicationSourceTLSClause(source.SQL.TLSProfile); tls != "" {
		b.WriteString(", ")
		b.WriteString(tls)
	}
	return b.String()
}

func replicationSourceTLSClause(tlsProfile string) string {
	switch strings.TrimSpace(strings.ToLower(tlsProfile)) {
	case "", "disabled", "off", "false":
		return ""
	default:
		return "SOURCE_SSL=1"
	}
}

func escapeSQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "'", "''")
}
