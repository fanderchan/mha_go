package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mha-go/internal/config"
	"mha-go/internal/controller/failover"
	"mha-go/internal/controller/monitor"
	"mha-go/internal/controller/switchover"
	"mha-go/internal/domain"
	"mha-go/internal/hooks"
	"mha-go/internal/obs"
	"mha-go/internal/state"
	"mha-go/internal/topology"
	sqltransport "mha-go/internal/transport/sql"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "--help" || os.Args[1] == "-h" || os.Args[1] == "help" {
		usage()
		return
	}

	ctx := context.Background()

	switch os.Args[1] {
	case "check-repl":
		os.Exit(runCheckRepl(ctx, os.Args[2:]))
	case "manager":
		os.Exit(runManager(ctx, os.Args[2:]))
	case "switch":
		os.Exit(runSwitch(ctx, os.Args[2:]))
	case "failover-plan":
		os.Exit(runFailoverPlan(ctx, os.Args[2:]))
	case "failover-execute":
		os.Exit(runFailoverExecute(ctx, os.Args[2:]))
	case "version":
		fmt.Printf("mha-go %s\n", version)
		return
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  mha --help
  mha check-repl       --config <file> [--discoverer sql|static] [--log-format text|json]
  mha manager          --config <file> [--discoverer sql|static] [--dry-run] [--log-format text|json]
  mha switch           --config <file> [--new-primary <node-id>] [--discoverer sql|static] [--dry-run] [--log-format text|json]
  mha failover-plan    --config <file> [--candidate <node-id>] [--discoverer sql|static] [--log-format text|json]
  mha failover-execute --config <file> [--candidate <node-id>] [--discoverer sql|static] [--dry-run] [--log-format text|json]
  mha version
`)
}

func runCheckRepl(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("check-repl", flag.ContinueOnError)
	configPath := fs.String("config", "", "cluster spec file")
	discovererName := fs.String("discoverer", "sql", "discoverer backend: static or sql")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	spec, logger, discoverer, store, _, err := bootstrap(*configPath, *logLevel, *logFormat, *discovererName)
	if err != nil {
		return reportErr(err)
	}

	engine := monitor.NewEngine(discoverer, store, logger)
	view, err := engine.CheckOnce(ctx, domain.RunKindCheckRepl, spec)
	if err != nil {
		return reportErr(err)
	}

	printView(view)
	assessment := topology.AssessReplication(spec, view)
	printAssessment(assessment)
	if assessment.HasErrors() {
		return 1
	}
	return 0
}

func runManager(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	configPath := fs.String("config", "", "cluster spec file (required)")
	discovererName := fs.String("discoverer", "sql", "discoverer backend: static or sql")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	dryRun := fs.Bool("dry-run", false, "plan and log failover steps without executing MySQL changes")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	spec, logger, discoverer, store, leases, err := bootstrap(*configPath, *logLevel, *logFormat, *discovererName)
	if err != nil {
		return reportErr(err)
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	inspector := sqltransport.NewMySQLInspector(sqltransport.NewRefResolver())
	engine := monitor.NewManagerEngine(discoverer, store, leases, inspector, buildHookDispatcher(spec, logger), logger)

	selector := topology.NewDefaultCandidateSelector()
	failCtrl := failover.NewController(discoverer, selector, leases, store, logger)
	var runner failover.ActionRunner
	if *dryRun {
		runner = failover.NewDryRunActionRunner(logger)
	} else {
		runner = failover.NewMySQLActionRunner(inspector, logger)
	}
	failExec := failover.NewExecutor(runner, leases, store, buildHookDispatcher(spec, logger), logger)
	handler := &failoverAdapter{ctrl: failCtrl, exec: failExec, logger: logger, dryRun: *dryRun}

	logger.Info("starting manager loop", "cluster", spec.Name, "dry_run", *dryRun)
	if err := engine.Run(runCtx, spec, handler); err != nil && !errors.Is(err, context.Canceled) {
		return reportErr(err)
	}
	return 0
}

type failoverAdapter struct {
	ctrl   *failover.Controller
	exec   *failover.Executor
	logger *obs.Logger
	dryRun bool
}

func (a *failoverAdapter) HandleFailover(ctx context.Context, spec domain.ClusterSpec, _ *domain.ClusterView) error {
	a.logger.Info("building failover plan", "cluster", spec.Name)
	plan, err := a.ctrl.BuildPlan(ctx, spec)
	if err != nil {
		return fmt.Errorf("build failover plan: %w", err)
	}
	execution, err := a.exec.ExecutePlan(ctx, spec, plan, a.dryRun)
	if execution != nil {
		printExecution(execution)
	}
	return err
}

func runSwitch(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	configPath := fs.String("config", "", "cluster spec file (required)")
	discovererName := fs.String("discoverer", "sql", "discoverer backend: static or sql")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	newPrimary := fs.String("new-primary", "", "node ID to promote (default: auto-select best candidate)")
	dryRun := fs.Bool("dry-run", false, "plan and log switchover steps without executing MySQL changes")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	spec, logger, discoverer, store, leases, err := bootstrap(*configPath, *logLevel, *logFormat, *discovererName)
	if err != nil {
		return reportErr(err)
	}

	var selector topology.CandidateSelector
	if *newPrimary != "" {
		selector = topology.NewPinnedCandidateSelector(*newPrimary)
	} else {
		selector = topology.NewDefaultCandidateSelector()
	}
	ctrl := switchover.NewController(discoverer, selector, store, logger)
	plan, err := ctrl.BuildPlan(ctx, spec)
	if err != nil {
		return reportErr(err)
	}
	printSwitchoverPlan(plan)

	inspector := sqltransport.NewMySQLInspector(sqltransport.NewRefResolver())
	var runner switchover.ActionRunner
	if *dryRun {
		runner = switchover.NewDryRunActionRunner(logger)
	} else {
		runner = switchover.NewMySQLActionRunner(inspector, logger)
	}

	exec := switchover.NewExecutor(runner, leases, store, buildHookDispatcher(spec, logger), logger)
	execution, err := exec.ExecutePlan(ctx, spec, plan, *dryRun)
	if execution != nil {
		printSwitchoverExecution(execution)
	}
	if err != nil {
		return reportErr(err)
	}
	return 0
}

func runFailoverPlan(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("failover-plan", flag.ContinueOnError)
	configPath := fs.String("config", "", "cluster spec file")
	discovererName := fs.String("discoverer", "sql", "discoverer backend: static or sql")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	candidate := fs.String("candidate", "", "node ID to use as failover candidate (default: auto-select)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	spec, logger, discoverer, store, leases, err := bootstrap(*configPath, *logLevel, *logFormat, *discovererName)
	if err != nil {
		return reportErr(err)
	}

	var selector topology.CandidateSelector
	if *candidate != "" {
		selector = topology.NewPinnedCandidateSelector(*candidate)
	} else {
		selector = topology.NewDefaultCandidateSelector()
	}
	controller := failover.NewController(discoverer, selector, leases, store, logger)
	plan, err := controller.BuildPlan(ctx, spec)
	if err != nil {
		return reportErr(err)
	}

	fmt.Printf("Failover plan for cluster %s\n", plan.ClusterName)
	fmt.Printf("  old primary:     %s\n", plan.OldPrimary.ID)
	fmt.Printf("  candidate:       %s\n", plan.Candidate.ID)
	fmt.Printf("  primary dead:    %t  (%s)\n", plan.PrimaryFailureConfirmed, plan.PrimaryFailureReason)
	fmt.Printf("  promote ready:   %t\n", plan.PromoteReadinessConfirmed)
	fmt.Printf("  execution:       %t\n", plan.ExecutionPermitted)
	fmt.Printf("  assess:          %d errors, %d warnings\n", plan.AssessmentErrors, plan.AssessmentWarnings)
	fmt.Printf("  salvage policy:  %s\n", plan.SalvagePolicy)
	if plan.MissingFromPrimaryKnown {
		fmt.Printf("  missing primary: %s\n", firstNonEmpty(plan.MissingFromPrimaryGTIDSet, "<none>"))
	} else {
		fmt.Println("  missing primary: <unknown>")
	}
	if len(plan.SalvageActions) > 0 {
		fmt.Println("  salvage actions:")
		for _, action := range plan.SalvageActions {
			fmt.Printf("    - kind=%s donor=%s required=%t\n", action.Kind, action.DonorNodeID, action.Required)
		}
	}
	if len(plan.BlockingReasons) > 0 {
		fmt.Println("  blocking reasons:")
		for _, reason := range plan.BlockingReasons {
			fmt.Printf("    - %s\n", reason)
		}
	}
	if len(plan.SkippedReplicaIDs) > 0 {
		fmt.Println("  skipped replicas:")
		for _, id := range plan.SkippedReplicaIDs {
			fmt.Printf("    - %s (unreachable during planning; rejoin later)\n", id)
		}
	}
	if len(plan.Steps) > 0 {
		fmt.Println("  steps:")
		for _, step := range plan.Steps {
			line := fmt.Sprintf("    - %-30s status=%s", step.Name, step.Status)
			if step.Blocking {
				line += " [BLOCKING]"
			}
			if step.Reason != "" {
				line += "  reason=" + step.Reason
			}
			fmt.Println(line)
		}
	}
	return 0
}

func runFailoverExecute(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("failover-execute", flag.ContinueOnError)
	configPath := fs.String("config", "", "cluster spec file")
	discovererName := fs.String("discoverer", "sql", "discoverer backend: static or sql")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	candidate := fs.String("candidate", "", "node ID to use as failover candidate (default: auto-select)")
	dryRun := fs.Bool("dry-run", false, "use dry-run action runner (no MySQL writes)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	spec, logger, discoverer, store, leases, err := bootstrap(*configPath, *logLevel, *logFormat, *discovererName)
	if err != nil {
		return reportErr(err)
	}

	var selector topology.CandidateSelector
	if *candidate != "" {
		selector = topology.NewPinnedCandidateSelector(*candidate)
	} else {
		selector = topology.NewDefaultCandidateSelector()
	}
	controller := failover.NewController(discoverer, selector, leases, store, logger)
	plan, err := controller.BuildPlan(ctx, spec)
	if err != nil {
		return reportErr(err)
	}

	var runner failover.ActionRunner
	if *dryRun {
		runner = failover.NewDryRunActionRunner(logger)
	} else {
		runner = failover.NewMySQLActionRunner(sqltransport.NewMySQLInspector(sqltransport.NewRefResolver()), logger)
	}
	executor := failover.NewExecutor(runner, leases, store, buildHookDispatcher(spec, logger), logger)
	execution, err := executor.ExecutePlan(ctx, spec, plan, *dryRun)
	if execution != nil {
		printExecution(execution)
	}
	if err != nil {
		return reportErr(err)
	}
	if execution.Blocked || !execution.Succeeded {
		return 1
	}
	return 0
}

func bootstrap(configPath, logLevel, logFormat, discovererName string) (domain.ClusterSpec, *obs.Logger, topology.Discoverer, state.RunStore, state.LeaseManager, error) {
	if configPath == "" {
		return domain.ClusterSpec{}, nil, nil, nil, nil, errors.New("--config must be set")
	}
	spec, err := config.LoadFile(configPath)
	if err != nil {
		return domain.ClusterSpec{}, nil, nil, nil, nil, err
	}
	logger := obs.NewLoggerWithFormat(logLevel, logFormat)
	store := state.NewMemoryStore()
	leases := state.NewLocalLeaseManager()
	var discoverer topology.Discoverer
	switch strings.ToLower(strings.TrimSpace(discovererName)) {
	case "", "static":
		discoverer = topology.NewStaticDiscoverer()
	case "sql":
		discoverer = topology.NewSQLDiscoverer(sqltransport.NewMySQLInspector(sqltransport.NewRefResolver()))
	default:
		return domain.ClusterSpec{}, nil, nil, nil, nil, fmt.Errorf("unsupported discoverer %q; use static or sql", discovererName)
	}
	return spec, logger, discoverer, store, leases, nil
}

func buildHookDispatcher(spec domain.ClusterSpec, logger *obs.Logger) hooks.Dispatcher {
	if spec.Hooks.ShellCompat && spec.Hooks.Command != "" {
		return hooks.NewShellDispatcher(spec.Hooks.Command, logger)
	}
	return hooks.NoopDispatcher{}
}

func reportErr(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

func printView(view *domain.ClusterView) {
	fmt.Printf("Cluster: %s  mode=%s  primary=%s  nodes=%d\n",
		view.ClusterName, view.TopologyKind, view.PrimaryID, len(view.Nodes))
	for _, node := range view.Nodes {
		fmt.Printf("  - %-6s role=%-7s health=%-7s addr=%-21s ro=%t sro=%t\n",
			node.ID, node.Role, node.Health, node.Address, node.ReadOnly, node.SuperReadOnly)
		if node.LastError != "" {
			fmt.Printf("         error: %s\n", node.LastError)
		}
		if node.Replica != nil {
			fmt.Printf("         replica: source=%s io=%t sql=%t lag=%ds autopos=%t\n",
				firstNonEmpty(node.Replica.SourceID, "unmapped"),
				node.Replica.IOThreadRunning, node.Replica.SQLThreadRunning,
				node.Replica.SecondsBehindSource, node.Replica.AutoPosition)
		}
	}
	for _, w := range view.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func printAssessment(assessment topology.Assessment) {
	if len(assessment.Findings) == 0 {
		fmt.Println("Assessment: OK")
		return
	}
	fmt.Println("Assessment:")
	for _, f := range assessment.Findings {
		node := ""
		if f.NodeID != "" {
			node = " node=" + f.NodeID
		}
		fmt.Printf("  [%s] %s%s: %s\n", f.Severity, f.Code, node, f.Message)
	}
}

func printSwitchoverPlan(plan *domain.SwitchoverPlan) {
	fmt.Printf("Switchover plan: %s → %s  endpoint_switch=%t\n",
		plan.OriginalPrimary.ID, plan.Candidate.ID, plan.RequiresWriterEndpointSwitch)
	for _, step := range plan.Steps {
		fmt.Printf("  - %-30s %s\n", step.Name, step.Status)
	}
}

func printSwitchoverExecution(execution *domain.SwitchoverExecution) {
	fmt.Printf("Switchover execution: cluster=%s dry_run=%t succeeded=%t\n",
		execution.ClusterName, execution.DryRun, execution.Succeeded)
	if execution.FailedStep != "" {
		fmt.Printf("  failed at: %s\n", execution.FailedStep)
	}
	for _, s := range execution.StepResults {
		line := fmt.Sprintf("  - %-30s %s", s.Name, s.Status)
		if s.Message != "" && s.Status != "completed" {
			line += "  " + s.Message
		}
		fmt.Println(line)
	}
}

func printExecution(execution *domain.FailoverExecution) {
	fmt.Printf("Failover execution: cluster=%s dry_run=%t succeeded=%t blocked=%t\n",
		execution.ClusterName, execution.DryRun, execution.Succeeded, execution.Blocked)
	if execution.FailedStep != "" {
		fmt.Printf("  failed at: %s\n", execution.FailedStep)
	}
	for _, step := range execution.StepResults {
		line := fmt.Sprintf("  - %-30s %s", step.Name, step.Status)
		if step.Message != "" {
			line += "  " + step.Message
		}
		fmt.Println(line)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
