package sql

import (
	"context"
	sqlstd "database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"mha-go/internal/capability"
	"mha-go/internal/domain"
)

type MySQLInspector struct {
	resolver       SecretResolver
	connectTimeout time.Duration
	queryTimeout   time.Duration
}

func NewMySQLInspector(resolver SecretResolver) *MySQLInspector {
	return &MySQLInspector{
		resolver:       resolver,
		connectTimeout: 3 * time.Second,
		queryTimeout:   3 * time.Second,
	}
}

// ResolvePassword resolves a password reference (e.g. env:VAR) for replication or admin use.
func (i *MySQLInspector) ResolvePassword(ctx context.Context, ref string) (string, error) {
	return i.resolver.Resolve(ctx, ref)
}

func (i *MySQLInspector) mysqlConfig(ctx context.Context, node domain.NodeSpec) (*gomysql.Config, error) {
	password, err := i.resolver.Resolve(ctx, node.SQL.PasswordRef)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials for node %q: %w", node.ID, err)
	}

	cfg := gomysql.NewConfig()
	cfg.User = node.SQL.User
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = node.Address()
	cfg.AllowNativePasswords = true
	cfg.InterpolateParams = true
	cfg.Timeout = i.connectTimeout
	cfg.ReadTimeout = i.queryTimeout
	cfg.WriteTimeout = i.queryTimeout

	tlsName, err := normalizeTLSProfile(node.SQL.TLSProfile)
	if err != nil {
		return nil, fmt.Errorf("node %q tls profile: %w", node.ID, err)
	}
	if tlsName != "" {
		cfg.TLSConfig = tlsName
	}
	return cfg, nil
}

// OpenDB opens a pooled connection to the node for administrative SQL. The caller must close the pool.
func (i *MySQLInspector) OpenDB(ctx context.Context, node domain.NodeSpec) (*sqlstd.DB, error) {
	cfg, err := i.mysqlConfig(ctx, node)
	if err != nil {
		return nil, err
	}
	db, err := sqlstd.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open MySQL connection for node %q: %w", node.ID, err)
	}
	pingCtx, cancelPing := context.WithTimeout(ctx, i.connectTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping node %q at %s: %w", node.ID, node.Address(), err)
	}
	return db, nil
}

func (i *MySQLInspector) Inspect(ctx context.Context, node domain.NodeSpec) (*Inspection, error) {
	cfg, err := i.mysqlConfig(ctx, node)
	if err != nil {
		return nil, err
	}

	db, err := sqlstd.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open MySQL connection for node %q: %w", node.ID, err)
	}
	defer db.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, i.connectTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping node %q at %s: %w", node.ID, node.Address(), err)
	}

	var (
		serverUUID     string
		version        string
		versionComment string
		gtidMode       string
		gtidExecuted   string
		readOnlyRaw    string
		superReadOnly  string
	)
	baseQueryCtx, cancelBaseQuery := context.WithTimeout(ctx, i.queryTimeout)
	defer cancelBaseQuery()
	if err := db.QueryRowContext(
		baseQueryCtx,
		`SELECT
			@@GLOBAL.server_uuid,
			@@GLOBAL.version,
			@@GLOBAL.version_comment,
			@@GLOBAL.gtid_mode,
			@@GLOBAL.gtid_executed,
			@@GLOBAL.read_only,
			@@GLOBAL.super_read_only`,
	).Scan(&serverUUID, &version, &versionComment, &gtidMode, &gtidExecuted, &readOnlyRaw, &superReadOnly); err != nil {
		return nil, fmt.Errorf("inspect base variables for node %q: %w", node.ID, err)
	}

	versionSeries, err := capability.NormalizeVersionSeries(version)
	if err != nil {
		return nil, fmt.Errorf("node %q returned unsupported version %q: %w", node.ID, version, err)
	}

	replicaQueryCtx, cancelReplicaQuery := context.WithTimeout(ctx, i.queryTimeout)
	defer cancelReplicaQuery()
	replicaRows, err := queryRowsMap(replicaQueryCtx, db, "SHOW REPLICA STATUS")
	if err != nil {
		return nil, fmt.Errorf("inspect replica status for node %q: %w", node.ID, err)
	}

	inspection := &Inspection{
		NodeID:         node.ID,
		Address:        node.Address(),
		ServerUUID:     serverUUID,
		Version:        version,
		VersionComment: versionComment,
		VersionSeries:  versionSeries,
		GTIDMode:       gtidMode,
		GTIDExecuted:   gtidExecuted,
		ReadOnly:       parseBoolish(readOnlyRaw),
		SuperReadOnly:  parseBoolish(superReadOnly),
	}

	for _, row := range replicaRows {
		channel := ReplicaChannelStatus{
			ChannelName:         rowValue(row, "Channel_Name", "Channel_name"),
			SourceHost:          rowValue(row, "Source_Host", "Master_Host"),
			SourcePort:          parseInt(rowValue(row, "Source_Port", "Master_Port")),
			SourceUUID:          rowValue(row, "Source_UUID", "Master_UUID"),
			AutoPosition:        parseBoolish(rowValue(row, "Auto_Position")),
			IOThreadRunning:     parseThreadRunning(rowValue(row, "Replica_IO_Running", "Slave_IO_Running")),
			SQLThreadRunning:    parseThreadRunning(rowValue(row, "Replica_SQL_Running", "Slave_SQL_Running")),
			RetrievedGTIDSet:    rowValue(row, "Retrieved_Gtid_Set"),
			ExecutedGTIDSet:     rowValue(row, "Executed_Gtid_Set"),
			SecondsBehindSource: parseNullableLag(rowValue(row, "Seconds_Behind_Source", "Seconds_Behind_Master")),
			LastIOError:         rowValue(row, "Last_IO_Error"),
			LastSQLError:        rowValue(row, "Last_SQL_Error"),
		}
		inspection.ReplicaChannels = append(inspection.ReplicaChannels, channel)
	}

	inspection.SemiSyncSourceEnabled = i.queryBoolishValueLike(ctx, db, "SHOW VARIABLES LIKE 'rpl_semi_sync_source_enabled'")
	inspection.SemiSyncSourceOperational = i.queryBoolishValueLike(ctx, db, "SHOW STATUS LIKE 'Rpl_semi_sync_source_status'")
	inspection.SemiSyncReplicaOperational = i.queryBoolishValueLike(ctx, db, "SHOW STATUS LIKE 'Rpl_semi_sync_replica_status'")

	return inspection, nil
}

func normalizeTLSProfile(profile string) (string, error) {
	value := strings.TrimSpace(strings.ToLower(profile))
	switch value {
	case "", "disabled", "off", "false":
		return "", nil
	case "default", "required", "true":
		return "true", nil
	case "preferred":
		return "preferred", nil
	case "skip-verify":
		return "skip-verify", nil
	default:
		return "", fmt.Errorf("unsupported tls profile %q", profile)
	}
}

func queryRowsMap(ctx context.Context, db *sqlstd.DB, query string) ([]map[string]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out := make([]map[string]string, 0)
	for rows.Next() {
		values := make([]sqlstd.RawBytes, len(columns))
		scanArgs := make([]any, len(columns))
		for i := range values {
			scanArgs[i] = &values[i]
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, err
		}

		row := make(map[string]string, len(columns))
		for i, column := range columns {
			row[column] = string(values[i])
		}
		out = append(out, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (i *MySQLInspector) queryBoolishValueLike(ctx context.Context, db *sqlstd.DB, query string) bool {
	queryCtx, cancel := context.WithTimeout(ctx, i.queryTimeout)
	defer cancel()

	rows, err := queryRowsMap(queryCtx, db, query)
	if err != nil || len(rows) == 0 {
		return false
	}
	return parseBoolish(rowValue(rows[0], "Value", "VARIABLE_VALUE"))
}

func rowValue(row map[string]string, keys ...string) string {
	for _, key := range keys {
		if value, ok := row[key]; ok {
			return value
		}
	}
	return ""
}

func parseBoolish(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "1", "on", "yes", "true":
		return true
	default:
		return false
	}
}

func parseThreadRunning(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "yes", "on":
		return true
	default:
		return false
	}
}

func parseInt(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return n
}

func parseNullableLag(value string) int64 {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" || trimmed == "null" {
		return -1
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
