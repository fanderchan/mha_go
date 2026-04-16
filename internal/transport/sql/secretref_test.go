package sql

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRefResolverResolve(t *testing.T) {
	t.Setenv("MHA_TEST_PASSWORD", "env-secret")

	dir := t.TempDir()
	path := filepath.Join(dir, "password.txt")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("write temp password file: %v", err)
	}

	resolver := NewRefResolver()
	ctx := context.Background()

	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr bool
	}{
		{name: "env", ref: "env:MHA_TEST_PASSWORD", want: "env-secret"},
		{name: "file", ref: "file:" + path, want: "file-secret"},
		{name: "plain", ref: "plain:inline-secret", want: "inline-secret"},
		{name: "unsupported", ref: "secret://mysql/repl", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolver.Resolve(ctx, tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for ref %q", tc.ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve %q: %v", tc.ref, err)
			}
			if got != tc.want {
				t.Fatalf("resolve %q = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}
