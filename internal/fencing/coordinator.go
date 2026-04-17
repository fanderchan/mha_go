package fencing

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	"mha-go/internal/writerendpoint"
)

const (
	KindReadOnly   = "read_only"
	KindCommand    = "command"
	KindVIP        = "vip"
	KindProxy      = "proxy"
	KindSTONITH    = "stonith"
	KindCloudRoute = "cloud_route"
)

// ReadOnlyFenceFunc performs the SQL-side read-only fence for the old primary.
type ReadOnlyFenceFunc func(context.Context) error

// Coordinator executes configured old-primary fencing steps.
type Coordinator struct {
	logger *obs.Logger
}

func NewCoordinator(logger *obs.Logger) *Coordinator {
	return &Coordinator{logger: logger}
}

// FenceOldPrimary executes all configured fencing steps in order.
//
// If no fencing section is configured, mha-go keeps the v1 default behavior:
// execute a required SQL read-only fence. Optional fence failures are logged but
// do not abort the failover; required failures return an error.
func (c *Coordinator) FenceOldPrimary(ctx context.Context, spec domain.ClusterSpec, oldPrimary, candidate domain.NodeState, readOnly ReadOnlyFenceFunc) error {
	steps := spec.Fencing.Steps
	if len(steps) == 0 {
		steps = []domain.FencingStepSpec{{Kind: KindReadOnly, Required: true}}
	}

	for _, step := range steps {
		kind := normalizeKind(step.Kind)
		if kind == "" {
			return fmt.Errorf("fencing step kind must be set")
		}
		err := c.executeStep(ctx, spec, oldPrimary, candidate, step, kind, readOnly)
		if err == nil {
			c.info("fencing step completed", "kind", kind, "old_primary", oldPrimary.ID)
			continue
		}
		if !step.Required {
			c.warn("optional fencing step failed, continuing", "kind", kind, "old_primary", oldPrimary.ID, "error", err)
			continue
		}
		return fmt.Errorf("required fencing step %q failed: %w", kind, err)
	}
	return nil
}

func (c *Coordinator) executeStep(
	ctx context.Context,
	spec domain.ClusterSpec,
	oldPrimary, candidate domain.NodeState,
	step domain.FencingStepSpec,
	kind string,
	readOnly ReadOnlyFenceFunc,
) error {
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}

	switch kind {
	case KindReadOnly:
		if readOnly == nil {
			return fmt.Errorf("read-only fence is not available")
		}
		return readOnly(ctx)
	case KindCommand, KindVIP, KindProxy, KindSTONITH, KindCloudRoute:
		if strings.TrimSpace(step.Command) == "" {
			return fmt.Errorf("fencing step %q requires command", kind)
		}
		return runCommand(ctx, step.Command, spec, oldPrimary, candidate, kind)
	default:
		return fmt.Errorf("unsupported fencing step kind %q", step.Kind)
	}
}

func runCommand(ctx context.Context, command string, spec domain.ClusterSpec, oldPrimary, candidate domain.NodeState, kind string) error {
	oldHost, oldPort := writerendpoint.AddrHostPort(oldPrimary.Address)
	newHost, newPort := writerendpoint.AddrHostPort(candidate.Address)
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Env = append(os.Environ(),
		"MHA_CLUSTER="+spec.Name,
		"MHA_FENCE_KIND="+kind,
		"MHA_OLD_PRIMARY_ID="+oldPrimary.ID,
		"MHA_OLD_PRIMARY_ADDRESS="+oldPrimary.Address,
		"MHA_OLD_PRIMARY_HOST="+oldHost,
		"MHA_OLD_PRIMARY_PORT="+strconv.Itoa(oldPort),
		"MHA_NEW_PRIMARY_ID="+candidate.ID,
		"MHA_NEW_PRIMARY_ADDRESS="+candidate.Address,
		"MHA_NEW_PRIMARY_HOST="+newHost,
		"MHA_NEW_PRIMARY_PORT="+strconv.Itoa(newPort),
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fencing command: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func normalizeKind(kind string) string {
	value := strings.TrimSpace(strings.ToLower(kind))
	value = strings.ReplaceAll(value, "-", "_")
	return value
}

func (c *Coordinator) info(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}

func (c *Coordinator) warn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}
