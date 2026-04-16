package hooks

import (
	"context"
	"os"
	"strings"
	"testing"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
)

func TestShellDispatcherSetsEnvVars(t *testing.T) {
	// Write the received env to a temp file so we can inspect it.
	tmp, err := os.CreateTemp("", "mha-hook-test-*")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	script := "env > " + tmp.Name()
	logger := obs.NewLogger("error")
	d := NewShellDispatcher(script, logger)

	event := Event{
		Name:    "failover.start",
		Cluster: "app1",
		RunKind: domain.RunKindFailover,
		NodeID:  "db1",
		Data:    map[string]string{"dead_primary": "db1"},
	}

	if err := d.Dispatch(context.Background(), event); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	content, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	envOutput := string(content)

	checks := map[string]string{
		"MHA_EVENT":       "failover.start",
		"MHA_CLUSTER":     "app1",
		"MHA_RUN_KIND":    "failover",
		"MHA_NODE_ID":     "db1",
		"MHA_DEAD_PRIMARY": "db1",
	}
	for key, want := range checks {
		found := false
		for _, line := range strings.Split(envOutput, "\n") {
			if strings.HasPrefix(line, key+"=") {
				val := strings.TrimPrefix(line, key+"=")
				val = strings.TrimSpace(val)
				if val == want {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("env var %s=%s not found in hook output:\n%s", key, want, envOutput)
		}
	}
}

func TestShellDispatcherReturnsErrorOnNonZeroExit(t *testing.T) {
	logger := obs.NewLogger("error")
	d := NewShellDispatcher("exit 1", logger)
	err := d.Dispatch(context.Background(), Event{Name: "test"})
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
}

func TestShellDispatcherNoopWhenCommandEmpty(t *testing.T) {
	logger := obs.NewLogger("error")
	d := NewShellDispatcher("", logger)
	if err := d.Dispatch(context.Background(), Event{Name: "test"}); err != nil {
		t.Fatalf("unexpected error with empty command: %v", err)
	}
}

func TestNoopDispatcher(t *testing.T) {
	var d Dispatcher = NoopDispatcher{}
	if err := d.Dispatch(context.Background(), Event{Name: "anything"}); err != nil {
		t.Fatalf("NoopDispatcher returned error: %v", err)
	}
}
