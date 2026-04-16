package sql

import (
	"context"
	"fmt"
	"os"
	"strings"
)

type RefResolver struct{}

func NewRefResolver() RefResolver {
	return RefResolver{}
}

func (RefResolver) Resolve(_ context.Context, ref string) (string, error) {
	value := strings.TrimSpace(ref)
	if value == "" {
		return "", nil
	}

	switch {
	case strings.HasPrefix(value, "env:"):
		key := strings.TrimSpace(strings.TrimPrefix(value, "env:"))
		if key == "" {
			return "", fmt.Errorf("password_ref %q is invalid: env key is empty", ref)
		}
		resolved, ok := os.LookupEnv(key)
		if !ok {
			return "", fmt.Errorf("password_ref %q is invalid: environment variable %q is not set", ref, key)
		}
		return resolved, nil
	case strings.HasPrefix(value, "file:"):
		path := strings.TrimSpace(strings.TrimPrefix(value, "file:"))
		if path == "" {
			return "", fmt.Errorf("password_ref %q is invalid: file path is empty", ref)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("resolve password_ref %q: %w", ref, err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	case strings.HasPrefix(value, "plain:"):
		return strings.TrimPrefix(value, "plain:"), nil
	default:
		return "", fmt.Errorf("password_ref %q is unsupported; use env:, file:, or plain:", ref)
	}
}
