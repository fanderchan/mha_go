package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mha-go/internal/domain"
)

const minimalYAML = `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    port: 3306
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    port: 3306
    version_series: "8.4"
    expected_role: replica
`

func writeTemp(t *testing.T, ext, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "mha-config-*"+ext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestLoadYAMLMinimal(t *testing.T) {
	path := writeTemp(t, ".yaml", minimalYAML)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if spec.Name != "app1" {
		t.Fatalf("Name = %q, want app1", spec.Name)
	}
	if len(spec.Nodes) != 2 {
		t.Fatalf("Nodes = %d, want 2", len(spec.Nodes))
	}
	primary := spec.Nodes[0]
	if primary.ID != "db1" || primary.ExpectedRole != domain.NodeRolePrimary {
		t.Fatalf("first node: id=%q role=%q", primary.ID, primary.ExpectedRole)
	}
}

func TestLoadTOML(t *testing.T) {
	toml := `
name = "app1"
[[nodes]]
id = "db1"
host = "10.0.0.1"
port = 3306
version_series = "8.4"
expected_role = "primary"
[[nodes]]
id = "db2"
host = "10.0.0.2"
port = 3306
version_series = "8.4"
expected_role = "replica"
`
	path := writeTemp(t, ".toml", toml)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile (TOML): %v", err)
	}
	if spec.Name != "app1" {
		t.Fatalf("Name = %q, want app1", spec.Name)
	}
}

func TestLoadJSON(t *testing.T) {
	jsonContent := `{
		"name": "app1",
		"nodes": [
			{"id": "db1", "host": "10.0.0.1", "port": 3306, "version_series": "8.4", "expected_role": "primary"},
			{"id": "db2", "host": "10.0.0.2", "port": 3306, "version_series": "8.4", "expected_role": "replica"}
		]
	}`
	path := writeTemp(t, ".json", jsonContent)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile (JSON): %v", err)
	}
	if spec.Name != "app1" {
		t.Fatalf("Name = %q, want app1", spec.Name)
	}
}

func TestLoadUnsupportedExtension(t *testing.T) {
	path := writeTemp(t, ".ini", "[cluster]")
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for .ini extension")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := LoadFile("/tmp/definitely-does-not-exist-mha.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidationMissingName(t *testing.T) {
	yaml := `
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error when name is missing")
	}
}

func TestValidationTooFewNodes(t *testing.T) {
	yaml := `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for fewer than 2 nodes")
	}
}

func TestValidationDuplicatePrimary(t *testing.T) {
	yaml := `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
    expected_role: primary
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for two primary nodes")
	}
}

func TestValidationUnsupportedVersionSeries(t *testing.T) {
	yaml := `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "5.7"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for unsupported version_series 5.7")
	}
}

func TestValidationInvalidSalvagePolicy(t *testing.T) {
	yaml := `
name: app1
replication:
  salvage:
    policy: unknown-policy
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for invalid salvage policy")
	}
}

func TestValidationInvalidSemiSyncPolicy(t *testing.T) {
	yaml := `
name: app1
replication:
  semi_sync:
    policy: bad-policy
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for invalid semi_sync policy")
	}
}

func TestValidationRejectsOldAsyncTopologyKind(t *testing.T) {
	yaml := `
name: app1
topology:
  kind: async-single-primary
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error for old async-single-primary topology kind")
	}
	if !strings.Contains(err.Error(), "use \"mysql-replication-single-primary\" instead") {
		t.Fatalf("error = %q, want rename guidance", err)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	path := writeTemp(t, ".yaml", minimalYAML)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	// Replication mode defaults to gtid.
	if spec.Replication.Mode != domain.ReplicationModeGTID {
		t.Fatalf("Replication.Mode = %q, want gtid", spec.Replication.Mode)
	}
	// Controller ID defaults to "controller-1".
	if spec.Controller.ID != "controller-1" {
		t.Fatalf("Controller.ID = %q, want controller-1", spec.Controller.ID)
	}
	// Topology kind defaults to mysql-replication-single-primary.
	if spec.Topology.Kind != domain.TopologyMySQLReplicationSinglePrimary {
		t.Fatalf("Topology.Kind = %q, want mysql-replication-single-primary", spec.Topology.Kind)
	}
	// First node becomes primary when none is declared.
	// (in minimalYAML we explicitly set it, so just check port default)
	for _, n := range spec.Nodes {
		if n.Port == 0 {
			t.Fatalf("node %q has zero port; should default to 3306", n.ID)
		}
	}
}

func TestLoadFencingAndWriterEndpointCommands(t *testing.T) {
	yaml := `
name: app1
fencing:
  steps:
    - kind: read_only
      required: true
    - kind: stonith
      required: false
      command: /usr/local/bin/fence-old-primary.sh
      timeout: 5s
writer_endpoint:
  kind: vip
  target: 192.0.2.10
  command: /usr/local/bin/move-vip.sh
  precheck_command: /usr/local/bin/check-vip.sh
  verify_command: /usr/local/bin/verify-vip.sh
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(spec.Fencing.Steps) != 2 {
		t.Fatalf("fencing steps = %d, want 2", len(spec.Fencing.Steps))
	}
	if spec.Fencing.Steps[1].Required {
		t.Fatal("second fencing step should be optional")
	}
	if spec.Fencing.Steps[1].Timeout.String() != "5s" {
		t.Fatalf("timeout = %s, want 5s", spec.Fencing.Steps[1].Timeout)
	}
	if spec.WriterEndpoint.PrecheckCommand == "" || spec.WriterEndpoint.VerifyCommand == "" {
		t.Fatalf("writer endpoint commands were not parsed: %+v", spec.WriterEndpoint)
	}
}

func TestLoadReplicationAndSSHFields(t *testing.T) {
	yaml := `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
    sql:
      user: mha
      password_ref: env:MHA_ADMIN_PASSWORD
      replication_user: repl
      replication_password_ref: env:MHA_REPL_PASSWORD
    ssh:
      user: mysql
      port: 2222
      private_key_ref: file:/etc/mha/id_ed25519
      binlog_dir: /mysql/binlogs
      binlog_index: /mysql/binlogs/mysql-bin.index
      binlog_prefix: mysql-bin
      mysqlbinlog_path: /usr/bin/mysqlbinlog
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	db1 := spec.Nodes[0]
	if db1.SQL.ReplicationUser != "repl" || db1.SQL.ReplicationPasswordRef != "env:MHA_REPL_PASSWORD" {
		t.Fatalf("replication credentials were not parsed: %+v", db1.SQL)
	}
	if db1.SSH == nil {
		t.Fatal("ssh config was not parsed")
	}
	if db1.SSH.Port != 2222 || db1.SSH.BinlogPrefix != "mysql-bin" || db1.SSH.MySQLBinlogPath != "/usr/bin/mysqlbinlog" {
		t.Fatalf("ssh binlog fields were not parsed: %+v", db1.SSH)
	}
}

func TestValidationPartialReplicationCredentials(t *testing.T) {
	yaml := `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
    expected_role: primary
    sql:
      user: mha
      password_ref: env:MHA_ADMIN_PASSWORD
      replication_user: repl
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error when only one replication credential field is set")
	}
}

func TestNoPrimaryDeclaredFirstNodeBecomePrimary(t *testing.T) {
	yaml := `
name: app1
nodes:
  - id: db1
    host: 10.0.0.1
    version_series: "8.4"
  - id: db2
    host: 10.0.0.2
    version_series: "8.4"
`
	path := writeTemp(t, ".yaml", yaml)
	spec, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if spec.Nodes[0].ExpectedRole != domain.NodeRolePrimary {
		t.Fatalf("first node role = %q, want primary", spec.Nodes[0].ExpectedRole)
	}
}

func TestLoadYMLExtension(t *testing.T) {
	// .yml is treated the same as .yaml
	path := filepath.Join(t.TempDir(), "cluster.yml")
	if err := os.WriteFile(path, []byte(minimalYAML), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatalf("LoadFile (.yml): %v", err)
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	for _, name := range []string{
		"cluster-8.4.yaml",
		"cluster-8.4.full.yaml",
		"cluster-test.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "examples", name)
			if _, err := LoadFile(path); err != nil {
				t.Fatalf("LoadFile(%s): %v", path, err)
			}
		})
	}
}
