package sql

import (
	"strings"
	"testing"

	"mha-go/internal/domain"
)

func TestBuildChangeReplicationSourceUsesReplicationCredentials(t *testing.T) {
	source := domain.NodeSpec{
		Host: "db-primary",
		Port: 3306,
		SQL: domain.SQLTargetSpec{
			User:                   "mha",
			PasswordRef:            "plain:admin",
			ReplicationUser:        "repl",
			ReplicationPasswordRef: "plain:repl",
		},
	}

	got := buildChangeReplicationSourceSQL(source, "secret")
	if !strings.Contains(got, "SOURCE_USER='repl'") {
		t.Fatalf("CHANGE REPLICATION SOURCE uses wrong user: %s", got)
	}
	if strings.Contains(got, "SOURCE_USER='mha'") {
		t.Fatalf("CHANGE REPLICATION SOURCE used admin user: %s", got)
	}
}

func TestBuildChangeReplicationSourceEscapesBackslashAndQuote(t *testing.T) {
	source := domain.NodeSpec{
		Host: `db\primary`,
		Port: 3306,
		SQL: domain.SQLTargetSpec{
			User: "repl",
		},
	}

	got := buildChangeReplicationSourceSQL(source, `p\' OR 1=1 --`)
	if !strings.Contains(got, `SOURCE_HOST='db\\primary'`) {
		t.Fatalf("host was not backslash-escaped: %s", got)
	}
	if !strings.Contains(got, `SOURCE_PASSWORD='p\\'' OR 1=1 --'`) {
		t.Fatalf("password was not safely escaped: %s", got)
	}
}
