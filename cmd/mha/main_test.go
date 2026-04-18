package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mha-cli-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tempdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	testBinary = filepath.Join(dir, "mha")
	build := exec.Command("go", "build", "-o", testBinary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build test binary: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(testBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("failed to start binary: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

func TestCLI_Version(t *testing.T) {
	stdout, _, code := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "mha-go") {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, "mha-go")
	}
}

func TestCLI_NoArgsShowsUsage(t *testing.T) {
	_, stderr, code := runCLI(t)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q, want usage text", stderr)
	}
}

func TestCLI_HelpShowsUsage(t *testing.T) {
	_, stderr, code := runCLI(t, "--help")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q, want usage text", stderr)
	}
}

func TestCLI_UnknownSubcommand(t *testing.T) {
	_, stderr, code := runCLI(t, "totally-not-a-command")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q, want usage text", stderr)
	}
}

func TestCLI_CheckReplMissingConfig(t *testing.T) {
	_, stderr, code := runCLI(t, "check-repl")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when --config is missing")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Fatalf("stderr is empty, want an error message about config")
	}
}

func TestCLI_FailoverPlanBadConfigPath(t *testing.T) {
	_, stderr, code := runCLI(t, "failover-plan", "--config", "/nonexistent/path/nope.yaml")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for missing config file")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Fatalf("stderr is empty, want an error about the missing file")
	}
}
