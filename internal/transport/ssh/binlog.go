package ssh

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mha-go/internal/domain"
)

type BinlogApplyOptions struct {
	OldPrimary        domain.NodeSpec
	Candidate         domain.NodeSpec
	CandidatePassword string
	IncludeGTIDSet    string
	ExcludeGTIDSet    string
	Timeout           time.Duration
	MySQLClientPath   string
}

func ApplyBinlogDump(ctx context.Context, executor StreamExecutor, opts BinlogApplyOptions) error {
	if executor == nil {
		return fmt.Errorf("SSH executor is not configured")
	}
	dumpCommand, err := buildBinlogDumpCommand(opts.OldPrimary, opts.IncludeGTIDSet, opts.ExcludeGTIDSet)
	if err != nil {
		return err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	mysqlPath := strings.TrimSpace(opts.MySQLClientPath)
	if mysqlPath == "" {
		mysqlPath = strings.TrimSpace(os.Getenv("MHA_MYSQL_CLIENT_PATH"))
	}
	if mysqlPath == "" {
		mysqlPath = "mysql"
	}
	cmd := exec.CommandContext(ctx, mysqlPath, mysqlClientArgs(opts.Candidate)...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+opts.CandidatePassword)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open mysql client stdin: %w", err)
	}
	var mysqlStdout, mysqlStderr bytes.Buffer
	cmd.Stdout = &mysqlStdout
	cmd.Stderr = &mysqlStderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mysql client %q: %w", mysqlPath, err)
	}

	_, streamErr := executor.Stream(ctx, opts.OldPrimary, dumpCommand, stdin)
	_ = stdin.Close()
	waitErr := cmd.Wait()

	if waitErr != nil {
		return fmt.Errorf("apply salvaged binlog to candidate %q: %w%s",
			opts.Candidate.ID, waitErr, commandOutputSuffix(mysqlStdout.String(), mysqlStderr.String()))
	}
	if streamErr != nil {
		return fmt.Errorf("dump old primary binlogs over SSH: %w", streamErr)
	}
	return nil
}

func buildBinlogDumpCommand(node domain.NodeSpec, includeGTIDSet, excludeGTIDSet string) (string, error) {
	if node.SSH == nil {
		return "", fmt.Errorf("node %q has no ssh config for binlog salvage", node.ID)
	}
	includeGTIDSet = strings.TrimSpace(includeGTIDSet)
	excludeGTIDSet = strings.TrimSpace(excludeGTIDSet)
	if includeGTIDSet != "" && excludeGTIDSet != "" {
		return "", fmt.Errorf("binlog salvage cannot use both include and exclude GTID filters")
	}
	if includeGTIDSet == "" && excludeGTIDSet == "" {
		return "", fmt.Errorf("binlog salvage requires include or exclude GTID filter")
	}

	binlogDir := firstNonEmpty(node.SSH.BinlogDir, "/var/lib/mysql")
	binlogIndex := strings.TrimSpace(node.SSH.BinlogIndex)
	binlogPrefix := firstNonEmpty(node.SSH.BinlogPrefix, "binlog")
	mysqlbinlog := firstNonEmpty(node.SSH.MySQLBinlogPath, "mysqlbinlog")

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("binlog_dir=" + shellQuote(binlogDir) + "\n")
	b.WriteString("binlog_index=" + shellQuote(binlogIndex) + "\n")
	b.WriteString("binlog_prefix=" + shellQuote(binlogPrefix) + "\n")
	b.WriteString("mysqlbinlog=" + shellQuote(mysqlbinlog) + "\n")
	b.WriteString("include_gtids=" + shellQuote(includeGTIDSet) + "\n")
	b.WriteString("exclude_gtids=" + shellQuote(excludeGTIDSet) + "\n")
	b.WriteString(`tmp="${TMPDIR:-/tmp}/mha-go-binlogs.$$"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
if [ -n "$binlog_index" ] && [ -r "$binlog_index" ]; then
	cat "$binlog_index" > "$tmp"
elif [ -r "$binlog_dir/${binlog_prefix}.index" ]; then
	cat "$binlog_dir/${binlog_prefix}.index" > "$tmp"
else
	find "$binlog_dir" -maxdepth 1 -type f -name "${binlog_prefix}.[0-9]*" | sort > "$tmp"
fi
set --
while IFS= read -r f; do
	[ -n "$f" ] || continue
	case "$f" in
		/*) path="$f" ;;
		./*) path="$binlog_dir/${f#./}" ;;
		*) path="$binlog_dir/$f" ;;
	esac
	if [ -f "$path" ]; then
		set -- "$@" "$path"
	fi
done < "$tmp"
if [ "$#" -eq 0 ]; then
	echo "mha-go: no binlog files found for prefix $binlog_prefix in $binlog_dir" >&2
	exit 2
fi
if [ -n "$include_gtids" ]; then
	exec "$mysqlbinlog" "--include-gtids=$include_gtids" "$@"
fi
exec "$mysqlbinlog" "--exclude-gtids=$exclude_gtids" "$@"
`)
	return b.String(), nil
}

func mysqlClientArgs(candidate domain.NodeSpec) []string {
	args := []string{
		"--protocol=tcp",
		"--host=" + candidate.Host,
		"--port=" + strconv.Itoa(candidate.Port),
		"--user=" + candidate.SQL.User,
		"--binary-mode",
		"--comments",
		"--connect-timeout=5",
	}
	if mode := mysqlClientSSLMode(candidate.SQL.TLSProfile); mode != "" {
		args = append(args, "--ssl-mode="+mode)
	}
	return args
}

func mysqlClientSSLMode(profile string) string {
	switch strings.TrimSpace(strings.ToLower(profile)) {
	case "", "disabled", "off", "false":
		return ""
	case "preferred":
		return "PREFERRED"
	default:
		return "REQUIRED"
	}
}

func commandOutputSuffix(stdout, stderr string) string {
	out := strings.TrimSpace(stdout)
	err := strings.TrimSpace(stderr)
	if out == "" && err == "" {
		return ""
	}
	var b strings.Builder
	if out != "" {
		b.WriteString("\nstdout:\n")
		b.WriteString(out)
	}
	if err != "" {
		b.WriteString("\nstderr:\n")
		b.WriteString(err)
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
