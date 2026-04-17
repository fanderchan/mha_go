package writerendpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"mha-go/internal/domain"
)

// Switch runs the writer endpoint command during a failover.
// kind "" / none / off: no-op. kind vip or proxy: requires writer_endpoint.command or env MHA_WRITER_ENDPOINT_COMMAND.
func Switch(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return SwitchWithNodes(ctx, spec, plan.Candidate, plan.OldPrimary)
}

// Precheck verifies that the writer endpoint can be switched before any
// irreversible failover action is taken. For vip/proxy endpoints it always
// checks that a switch command exists, then runs the optional precheck command.
func Precheck(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return PrecheckWithNodes(ctx, spec, plan.Candidate, plan.OldPrimary)
}

// Verify checks the writer endpoint after it has been switched. It is a no-op
// unless writer_endpoint.verify_command or MHA_WRITER_ENDPOINT_VERIFY_COMMAND is set.
func Verify(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return VerifyWithNodes(ctx, spec, plan.Candidate, plan.OldPrimary)
}

// SwitchForSwitchover runs the writer endpoint command during an online switchover.
func SwitchForSwitchover(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	return SwitchWithNodes(ctx, spec, plan.Candidate, plan.OriginalPrimary)
}

func PrecheckWithNodes(ctx context.Context, spec domain.ClusterSpec, newPrimary, oldPrimary domain.NodeState) error {
	kind := strings.ToLower(strings.TrimSpace(spec.WriterEndpoint.Kind))
	switch kind {
	case "", "none", "off":
		return nil
	case "vip", "proxy":
		if _, err := switchCommand(spec); err != nil {
			return err
		}
		cmd := strings.TrimSpace(spec.WriterEndpoint.PrecheckCommand)
		if cmd == "" {
			cmd = strings.TrimSpace(os.Getenv("MHA_WRITER_ENDPOINT_PRECHECK_COMMAND"))
		}
		if cmd == "" {
			return nil
		}
		return runScript(ctx, "precheck", cmd, spec, newPrimary, oldPrimary)
	default:
		return fmt.Errorf("unsupported writer_endpoint kind %q", spec.WriterEndpoint.Kind)
	}
}

func VerifyWithNodes(ctx context.Context, spec domain.ClusterSpec, newPrimary, oldPrimary domain.NodeState) error {
	kind := strings.ToLower(strings.TrimSpace(spec.WriterEndpoint.Kind))
	switch kind {
	case "", "none", "off":
		return nil
	case "vip", "proxy":
		cmd := strings.TrimSpace(spec.WriterEndpoint.VerifyCommand)
		if cmd == "" {
			cmd = strings.TrimSpace(os.Getenv("MHA_WRITER_ENDPOINT_VERIFY_COMMAND"))
		}
		if cmd == "" {
			return nil
		}
		return runScript(ctx, "verify", cmd, spec, newPrimary, oldPrimary)
	default:
		return fmt.Errorf("unsupported writer_endpoint kind %q", spec.WriterEndpoint.Kind)
	}
}

// SwitchWithNodes runs the external writer-endpoint command with newPrimary and oldPrimary
// passed as environment variables. It is shared between failover and switchover paths.
func SwitchWithNodes(ctx context.Context, spec domain.ClusterSpec, newPrimary, oldPrimary domain.NodeState) error {
	kind := strings.ToLower(strings.TrimSpace(spec.WriterEndpoint.Kind))
	switch kind {
	case "", "none", "off":
		return nil
	case "vip", "proxy":
		cmd, err := switchCommand(spec)
		if err != nil {
			return err
		}
		return runScript(ctx, "switch", cmd, spec, newPrimary, oldPrimary)
	default:
		return fmt.Errorf("unsupported writer_endpoint kind %q", spec.WriterEndpoint.Kind)
	}
}

func switchCommand(spec domain.ClusterSpec) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(spec.WriterEndpoint.Kind))
	cmd := strings.TrimSpace(spec.WriterEndpoint.Command)
	if cmd == "" {
		cmd = strings.TrimSpace(os.Getenv("MHA_WRITER_ENDPOINT_COMMAND"))
	}
	if cmd == "" {
		return "", fmt.Errorf("writer_endpoint kind %q requires writer_endpoint.command or env MHA_WRITER_ENDPOINT_COMMAND", kind)
	}
	return cmd, nil
}

func runScript(ctx context.Context, action, script string, spec domain.ClusterSpec, newPrimary, oldPrimary domain.NodeState) error {
	newHost, newPort := AddrHostPort(newPrimary.Address)
	oldHost, oldPort := AddrHostPort(oldPrimary.Address)
	c := exec.CommandContext(ctx, "sh", "-c", script)
	c.Env = append(os.Environ(),
		"MHA_CLUSTER="+spec.Name,
		"MHA_WRITER_ENDPOINT_ACTION="+action,
		"MHA_WRITER_ENDPOINT_KIND="+spec.WriterEndpoint.Kind,
		"MHA_WRITER_ENDPOINT_TARGET="+spec.WriterEndpoint.Target,
		"MHA_NEW_PRIMARY_ID="+newPrimary.ID,
		"MHA_NEW_PRIMARY_ADDRESS="+newPrimary.Address,
		"MHA_NEW_PRIMARY_HOST="+newHost,
		"MHA_NEW_PRIMARY_PORT="+strconv.Itoa(newPort),
		"MHA_OLD_PRIMARY_ID="+oldPrimary.ID,
		"MHA_OLD_PRIMARY_ADDRESS="+oldPrimary.Address,
		"MHA_OLD_PRIMARY_HOST="+oldHost,
		"MHA_OLD_PRIMARY_PORT="+strconv.Itoa(oldPort),
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("writer endpoint %s script: %w\n%s", action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AddrHostPort splits "host:port" from NodeState.Address for env consumers.
func AddrHostPort(addr string) (host string, port int) {
	host = addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			port = p
		}
	}
	return host, port
}
