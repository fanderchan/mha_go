package monitor

import (
	"context"
	"fmt"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/hooks"
	"mha-go/internal/obs"
	"mha-go/internal/state"
	"mha-go/internal/topology"
	sqltransport "mha-go/internal/transport/sql"
)

// FailoverHandler is invoked by the monitor loop when the primary is confirmed dead.
type FailoverHandler interface {
	HandleFailover(ctx context.Context, spec domain.ClusterSpec, view *domain.ClusterView) error
}

// monitorPhase represents the current state of the monitoring state machine.
type monitorPhase int

const (
	phaseHealthy           monitorPhase = iota // primary is responding normally
	phaseSuspect                               // one or more consecutive probe failures, below threshold
	phaseSecondaryCheck                        // failure threshold reached; running secondary verification
	phaseReconfirmTopology                     // secondary check failed; re-discovering full topology
	phaseDeadConfirmed                         // primary confirmed dead; ready to trigger failover
)

func (p monitorPhase) String() string {
	switch p {
	case phaseHealthy:
		return "healthy"
	case phaseSuspect:
		return "suspect"
	case phaseSecondaryCheck:
		return "secondary-check"
	case phaseReconfirmTopology:
		return "reconfirm-topology"
	case phaseDeadConfirmed:
		return "dead-confirmed"
	default:
		return "unknown"
	}
}

// Engine performs topology discovery and drives the monitoring state machine.
type Engine struct {
	discoverer topology.Discoverer
	store      state.RunStore
	logger     *obs.Logger
	dispatcher hooks.Dispatcher
	// set only by NewManagerEngine; required for Run()
	inspector *sqltransport.MySQLInspector
	leases    state.LeaseManager
	// injectable for testing; if nil, the real implementations are used
	probePrimary           func(ctx context.Context, ns domain.NodeSpec) error
	runSecondaryChecksFunc func(ctx context.Context, spec domain.ClusterSpec, view *domain.ClusterView) bool
}

// NewEngine creates a lightweight engine suitable for single-shot checks (check-repl).
func NewEngine(discoverer topology.Discoverer, store state.RunStore, logger *obs.Logger) *Engine {
	return &Engine{
		discoverer: discoverer,
		store:      store,
		logger:     logger,
	}
}

// NewManagerEngine creates an Engine capable of running the long-running monitoring loop.
// dispatcher may be nil; in that case hook events are silently discarded.
func NewManagerEngine(
	discoverer topology.Discoverer,
	store state.RunStore,
	leases state.LeaseManager,
	inspector *sqltransport.MySQLInspector,
	dispatcher hooks.Dispatcher,
	logger *obs.Logger,
) *Engine {
	if dispatcher == nil {
		dispatcher = hooks.NoopDispatcher{}
	}
	return &Engine{
		discoverer: discoverer,
		store:      store,
		logger:     logger,
		dispatcher: dispatcher,
		inspector:  inspector,
		leases:     leases,
	}
}

// CheckOnce performs a single discovery cycle and records it to the store.
func (e *Engine) CheckOnce(ctx context.Context, kind domain.RunKind, spec domain.ClusterSpec) (*domain.ClusterView, error) {
	run, err := e.store.CreateRun(ctx, domain.RunRecord{
		Cluster: spec.Name,
		Kind:    kind,
		Status:  domain.RunStatusRunning,
	})
	if err != nil {
		return nil, err
	}

	_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
		Phase:    "discover",
		Severity: domain.EventSeverityInfo,
		Message:  "starting cluster discovery",
	})

	view, err := e.discoverer.Discover(ctx, spec)
	if err != nil {
		_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
			Phase:    "discover",
			Severity: domain.EventSeverityError,
			Message:  err.Error(),
		})
		_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, err
	}

	summary := fmt.Sprintf("discovered %d nodes, primary=%s", len(view.Nodes), view.PrimaryID)
	_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
		Phase:    "discover",
		Severity: domain.EventSeverityInfo,
		Message:  summary,
		Metadata: map[string]string{
			"collected_at": view.CollectedAt.Format(time.RFC3339),
		},
	})
	_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusSucceeded, summary)
	e.logger.Info("cluster discovery completed", "cluster", spec.Name, "primary", view.PrimaryID, "nodes", len(view.Nodes))
	return view, nil
}

// Run starts the long-running monitoring loop. It blocks until ctx is cancelled, a fatal
// error occurs, or the failover handler returns. The caller must use NewManagerEngine.
//
// State machine:
//
//	Healthy ──probe fail──► Suspect ──threshold──► SecondaryCheck
//	   ▲                        │                       │
//	   └──────────────────── alive ◄──────────────── alive
//	                             │
//	                          all fail
//	                             │
//	                             ▼
//	                     ReconfirmTopology ──dead──► DeadConfirmed ──► failover
//	                             │
//	                          alive
//	                             │
//	                             ▼
//	                           Healthy
func (e *Engine) Run(ctx context.Context, spec domain.ClusterSpec, handler FailoverHandler) error {
	if e.leases == nil {
		return fmt.Errorf("Run requires a lease manager; use NewManagerEngine")
	}
	if e.inspector == nil && e.probePrimary == nil {
		return fmt.Errorf("Run requires an inspector or a probePrimary hook; use NewManagerEngine")
	}

	leaseKey := "manager/" + spec.Name
	lease, err := e.leases.Acquire(ctx, leaseKey, spec.Controller.ID, spec.Controller.Lease.TTL)
	if err != nil {
		return fmt.Errorf("acquire manager lease for cluster %q: %w", spec.Name, err)
	}
	defer func() { _ = lease.Release(context.Background()) }()

	e.logger.Info("manager started",
		"cluster", spec.Name,
		"controller", spec.Controller.ID,
		"interval", spec.Controller.Monitor.Interval,
		"failure_threshold", spec.Controller.Monitor.FailureThreshold,
	)

	// Create a long-lived RunRecord for this manager session.
	run, err := e.store.CreateRun(ctx, domain.RunRecord{
		Cluster: spec.Name,
		Kind:    domain.RunKindMonitor,
		Status:  domain.RunStatusRunning,
	})
	if err != nil {
		return fmt.Errorf("create monitor run record: %w", err)
	}
	appendEvent := func(phase string, sev domain.EventSeverity, msg string, meta map[string]string) {
		_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
			Phase:    phase,
			Severity: sev,
			Message:  msg,
			Metadata: meta,
		})
	}

	// Initial topology discovery.
	view, err := e.discoverer.Discover(ctx, spec)
	if err != nil {
		_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return fmt.Errorf("initial topology discovery failed: %w", err)
	}
	e.logger.Info("initial topology discovered",
		"cluster", spec.Name,
		"primary", view.PrimaryID,
		"nodes", len(view.Nodes),
	)
	appendEvent("init", domain.EventSeverityInfo, "monitoring loop started",
		map[string]string{"primary": view.PrimaryID})

	phase := phaseHealthy
	failureCount := 0
	threshold := spec.Controller.Monitor.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}

	ticker := time.NewTicker(spec.Controller.Monitor.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.logger.Info("monitor loop stopping", "cluster", spec.Name, "reason", ctx.Err())
			_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusAborted, "context cancelled")
			return ctx.Err()
		case <-ticker.C:
		}

		prevPhase := phase
		phase, failureCount, view = e.step(ctx, spec, view, phase, failureCount, threshold)

		if phase != prevPhase {
			e.logger.Info("monitor phase transition",
				"cluster", spec.Name,
				"from", prevPhase,
				"to", phase,
				"primary", view.PrimaryID,
			)
			appendEvent("monitor", phaseSeverity(phase),
				fmt.Sprintf("phase transition: %s → %s", prevPhase, phase),
				map[string]string{
					"primary":       view.PrimaryID,
					"failure_count": fmt.Sprint(failureCount),
				})
			// Dispatch hook events on significant transitions.
			switch phase {
			case phaseSuspect:
				_ = e.dispatcher.Dispatch(ctx, hooks.Event{
					Name:    "monitor.suspect",
					Cluster: spec.Name,
					RunKind: domain.RunKindMonitor,
					NodeID:  view.PrimaryID,
					Data: map[string]string{
						"primary":       view.PrimaryID,
						"failure_count": fmt.Sprint(failureCount),
					},
				})
			case phaseDeadConfirmed:
				_ = e.dispatcher.Dispatch(ctx, hooks.Event{
					Name:    "monitor.dead_confirmed",
					Cluster: spec.Name,
					RunKind: domain.RunKindMonitor,
					NodeID:  view.PrimaryID,
					Data:    map[string]string{"primary": view.PrimaryID},
				})
			}
		}

		if phase == phaseDeadConfirmed {
			e.logger.Error("primary dead confirmed, triggering failover",
				"cluster", spec.Name,
				"dead_primary", view.PrimaryID,
			)
			appendEvent("failover", domain.EventSeverityError,
				"primary dead confirmed, handing over to failover controller",
				map[string]string{"dead_primary": view.PrimaryID})

			if handler == nil {
				_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, "no failover handler configured")
				return fmt.Errorf("primary %q confirmed dead but no failover handler is configured", view.PrimaryID)
			}
			foErr := handler.HandleFailover(ctx, spec, view)
			if foErr != nil {
				_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, foErr.Error())
				return fmt.Errorf("failover handler: %w", foErr)
			}
			_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusSucceeded, "failover completed")
			return nil
		}
	}
}

// step executes one iteration of the monitoring state machine and returns the new phase,
// updated failure count, and the (possibly refreshed) cluster view.
func (e *Engine) step(
	ctx context.Context,
	spec domain.ClusterSpec,
	view *domain.ClusterView,
	phase monitorPhase,
	failureCount, threshold int,
) (monitorPhase, int, *domain.ClusterView) {
	switch phase {

	case phaseHealthy, phaseSuspect:
		primarySpec, ok := nodeSpecByID(spec, view.PrimaryID)
		if !ok {
			e.logger.Warn("primary node not found in cluster config, skipping probe", "primary", view.PrimaryID)
			return phase, failureCount, view
		}
		probeFn := e.probePrimary
		if probeFn == nil {
			probeFn = func(ctx context.Context, ns domain.NodeSpec) error {
				return pingPrimary(ctx, e.inspector, ns)
			}
		}
		if err := probeFn(ctx, primarySpec); err == nil {
			if phase == phaseSuspect {
				e.logger.Info("primary recovered",
					"primary", view.PrimaryID,
					"previous_failures", failureCount,
				)
			}
			return phaseHealthy, 0, view
		} else {
			failureCount++
			e.logger.Warn("primary probe failed",
				"primary", view.PrimaryID,
				"failures", failureCount,
				"threshold", threshold,
				"error", err,
			)
			if failureCount >= threshold {
				return phaseSecondaryCheck, failureCount, view
			}
			return phaseSuspect, failureCount, view
		}

	case phaseSecondaryCheck:
		secondaryFn := e.runSecondaryChecksFunc
		if secondaryFn == nil {
			secondaryFn = e.runSecondaryChecks
		}
		if secondaryFn(ctx, spec, view) {
			e.logger.Info("secondary check: primary reachable from replica(s), resetting to healthy",
				"primary", view.PrimaryID,
			)
			return phaseHealthy, 0, view
		}
		e.logger.Warn("secondary check: primary unreachable from all observers",
			"primary", view.PrimaryID,
		)
		return phaseReconfirmTopology, failureCount, view

	case phaseReconfirmTopology:
		timeout := spec.Controller.Monitor.ReconfirmTimeout
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		reconfirmCtx, cancel := context.WithTimeout(ctx, timeout)
		newView, err := e.discoverer.Discover(reconfirmCtx, spec)
		cancel()
		if err != nil {
			e.logger.Error("topology reconfirmation discovery failed, treating primary as dead",
				"primary", view.PrimaryID,
				"error", err,
			)
			return phaseDeadConfirmed, failureCount, view
		}
		primaryNode, ok := newView.PrimaryNode()
		if !ok || primaryNode.Health == domain.NodeHealthDead {
			e.logger.Error("primary confirmed dead after topology reconfirmation",
				"primary", newView.PrimaryID,
			)
			return phaseDeadConfirmed, failureCount, newView
		}
		e.logger.Info("primary alive after topology reconfirmation, resetting to healthy",
			"primary", newView.PrimaryID,
		)
		return phaseHealthy, 0, newView
	}

	return phase, failureCount, view
}

// runSecondaryChecks tries to confirm primary reachability through other cluster nodes.
// It first checks whether any replica's IO thread is still connected to the primary, then
// consults any explicitly configured secondary-check observers.
// Returns true if at least one secondary source confirms the primary is alive.
func (e *Engine) runSecondaryChecks(ctx context.Context, spec domain.ClusterSpec, view *domain.ClusterView) bool {
	primarySpec, ok := nodeSpecByID(spec, view.PrimaryID)
	if !ok {
		return false
	}

	// 1. Ask each alive replica whether its IO thread still points at the primary.
	for _, replica := range view.ReplicaNodes() {
		if replica.Health == domain.NodeHealthDead {
			continue
		}
		replicaSpec, ok := nodeSpecByID(spec, replica.ID)
		if !ok {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		sees, err := replicaSeesSource(checkCtx, e.inspector, replicaSpec, primarySpec.Host, primarySpec.Port)
		cancel()
		if err != nil {
			e.logger.Warn("secondary check: could not inspect replica",
				"replica", replica.ID, "error", err)
			continue
		}
		if sees {
			e.logger.Info("secondary check: replica IO thread still connected to primary",
				"replica", replica.ID, "primary", view.PrimaryID)
			return true
		}
	}

	// 2. Consult explicitly configured secondary-check observer nodes.
	for _, sc := range spec.Controller.SecondaryChecks {
		if sc.ObserverNode == "" {
			continue
		}
		obsSpec, ok := nodeSpecByID(spec, sc.ObserverNode)
		if !ok {
			e.logger.Warn("secondary check: observer node not found in config", "observer", sc.ObserverNode)
			continue
		}
		timeout := sc.Timeout
		if timeout <= 0 {
			timeout = 2 * time.Second
		}
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		sees, err := replicaSeesSource(checkCtx, e.inspector, obsSpec, primarySpec.Host, primarySpec.Port)
		cancel()
		if err != nil {
			e.logger.Warn("secondary check: could not inspect observer",
				"observer", sc.ObserverNode, "error", err)
			continue
		}
		if sees {
			e.logger.Info("secondary check: observer confirms primary reachable",
				"observer", sc.ObserverNode, "primary", view.PrimaryID)
			return true
		}
	}

	return false
}

// phaseSeverity maps a monitor phase to an appropriate log event severity.
func phaseSeverity(phase monitorPhase) domain.EventSeverity {
	switch phase {
	case phaseHealthy:
		return domain.EventSeverityInfo
	case phaseSuspect, phaseSecondaryCheck, phaseReconfirmTopology:
		return domain.EventSeverityWarn
	default:
		return domain.EventSeverityError
	}
}
