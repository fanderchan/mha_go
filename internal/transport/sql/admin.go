package sql

import (
	"context"
	sqlstd "database/sql"
	"fmt"
	"strings"

	"mha-go/internal/domain"
)

// GetGTIDExecuted returns the current @@GLOBAL.gtid_executed set on the instance.
func GetGTIDExecuted(ctx context.Context, db *sqlstd.DB) (string, error) {
	var gtid string
	if err := db.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_executed`).Scan(&gtid); err != nil {
		return "", fmt.Errorf("get gtid_executed: %w", err)
	}
	return strings.TrimSpace(gtid), nil
}

// FenceReadOnly enables super_read_only/read_only on the instance to block writes (SQL-side fencing).
func FenceReadOnly(ctx context.Context, db *sqlstd.DB, caps domain.CapabilitySet) error {
	if caps.SupportsReadOnlyFence && caps.HasSuperReadOnly {
		if _, err := db.ExecContext(ctx, `SET GLOBAL super_read_only = ON`); err == nil {
			return nil
		}
	}
	if _, err := db.ExecContext(ctx, `SET GLOBAL read_only = ON`); err != nil {
		return fmt.Errorf("set read_only: %w", err)
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
	user := escapeSQLString(source.SQL.User)
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
	return strings.ReplaceAll(s, "'", "''")
}
