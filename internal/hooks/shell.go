package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"mha-go/internal/obs"
)

// ShellDispatcher executes a shell command for every hook event.
// Environment variables are set from the event fields plus any Data entries
// prefixed with MHA_.
type ShellDispatcher struct {
	command string
	logger  *obs.Logger
}

// NewShellDispatcher creates a dispatcher that runs command for each event.
// command is executed via "sh -c <command>".
func NewShellDispatcher(command string, logger *obs.Logger) *ShellDispatcher {
	return &ShellDispatcher{command: command, logger: logger}
}

func (d *ShellDispatcher) Dispatch(ctx context.Context, event Event) error {
	if strings.TrimSpace(d.command) == "" {
		return nil
	}
	c := exec.CommandContext(ctx, "sh", "-c", d.command)
	c.Env = append(os.Environ(), buildEnv(event)...)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hook %q: %w\n%s", event.Name, err, strings.TrimSpace(string(out)))
	}
	d.logger.Info("hook dispatched", "event", event.Name, "cluster", event.Cluster)
	return nil
}

func buildEnv(event Event) []string {
	env := []string{
		"MHA_EVENT=" + event.Name,
		"MHA_CLUSTER=" + event.Cluster,
		"MHA_RUN_KIND=" + string(event.RunKind),
		"MHA_NODE_ID=" + event.NodeID,
	}
	for k, v := range event.Data {
		env = append(env, "MHA_"+strings.ToUpper(k)+"="+v)
	}
	return env
}

// NoopDispatcher silently discards all events. Used when hooks are not configured.
type NoopDispatcher struct{}

func (NoopDispatcher) Dispatch(_ context.Context, _ Event) error { return nil }
