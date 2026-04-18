package ssh

import (
	"strings"
	"testing"

	"mha-go/internal/domain"
)

func TestBuildBinlogDumpCommandUsesIncludeGTIDsAndConfiguredPaths(t *testing.T) {
	node := domain.NodeSpec{
		ID: "db1",
		SSH: &domain.SSHTargetSpec{
			BinlogDir:       "/mysql/binlogs",
			BinlogIndex:     "/mysql/binlogs/mysql-bin.index",
			BinlogPrefix:    "mysql-bin",
			MySQLBinlogPath: "/usr/bin/mysqlbinlog",
		},
	}

	got, err := buildBinlogDumpCommand(node, "uuid:1-10", "")
	if err != nil {
		t.Fatalf("buildBinlogDumpCommand: %v", err)
	}
	for _, want := range []string{
		"binlog_dir='/mysql/binlogs'",
		"binlog_index='/mysql/binlogs/mysql-bin.index'",
		"binlog_prefix='mysql-bin'",
		"mysqlbinlog='/usr/bin/mysqlbinlog'",
		`"--include-gtids=$include_gtids"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command missing %q:\n%s", want, got)
		}
	}
}

func TestBuildBinlogDumpCommandUsesExcludeGTIDsForUnknownGap(t *testing.T) {
	node := domain.NodeSpec{
		ID:  "db1",
		SSH: &domain.SSHTargetSpec{},
	}

	got, err := buildBinlogDumpCommand(node, "", "uuid:1-10")
	if err != nil {
		t.Fatalf("buildBinlogDumpCommand: %v", err)
	}
	if !strings.Contains(got, `exec "$mysqlbinlog" "--exclude-gtids=$exclude_gtids" "$@"`) {
		t.Fatalf("command does not use exclude-gtids:\n%s", got)
	}
}

func TestBuildBinlogDumpCommandRejectsMissingFilter(t *testing.T) {
	node := domain.NodeSpec{ID: "db1", SSH: &domain.SSHTargetSpec{}}
	if _, err := buildBinlogDumpCommand(node, "", ""); err == nil {
		t.Fatal("expected error without include/exclude GTID filter")
	}
}
