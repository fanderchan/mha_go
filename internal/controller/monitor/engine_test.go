package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	"mha-go/internal/state"
	"mha-go/internal/topology"
	sqltransport "mha-go/internal/transport/sql"
)

// --- test doubles ---

type fakeDiscoverer struct {
	views []*domain.ClusterView
	errs  []error
	calls int
}

func (f *fakeDiscoverer) Discover(_ context.Context, spec domain.ClusterSpec) (*domain.ClusterView, error) {
	i := f.calls
	if i >= len(f.views) {
		i = len(f.views) - 1
	}
	f.calls++
	return f.views[i], f.errs[i]
}

func singleView(primaryHealth domain.NodeHealth) *domain.ClusterView {
	return &domain.ClusterView{
		ClusterName: "test",
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: primaryHealth},
			{ID: "db2", Role: domain.NodeRoleReplica, Health: domain.NodeHealthAlive,
				Replica: &domain.ReplicaState{SourceID: "db1", IOThreadRunning: true, SQLThreadRunning: true, AutoPosition: true}},
		},
	}
}

type fakeInspector struct {
	// nodeID -> error returned by OpenDB/Inspect (nil means success)
	openDBErrors  map[string]error
	inspectErrors map[string]error
	// replicaChannels returned by Inspect per nodeID
	channels map[string][]sqltransport.ReplicaChannelStatus
}

func (f *fakeInspector) openDB(_ context.Context, ns domain.NodeSpec) error {
	if f.openDBErrors == nil {
		return nil
	}
	return f.openDBErrors[ns.ID]
}

func (f *fakeInspector) inspect(_ context.Context, ns domain.NodeSpec) (*sqltransport.Inspection, error) {
	if f.inspectErrors != nil {
		if err := f.inspectErrors[ns.ID]; err != nil {
			return nil, err
		}
	}
	var chs []sqltransport.ReplicaChannelStatus
	if f.channels != nil {
		chs = f.channels[ns.ID]
	}
	return &sqltransport.Inspection{NodeID: ns.ID, ReplicaChannels: chs}, nil
}

// wrapInspector adapts fakeInspector to the functions used by the engine.
// We inject it by swapping the engine's probing functions via a test-only hook.

type fakeLeases struct{}

func (f *fakeLeases) Acquire(_ context.Context, key, owner string, _ time.Duration) (state.LeaseHandle, error) {
	return &fakeLeaseHandle{key: key, owner: owner}, nil
}

type fakeLeaseHandle struct{ key, owner string }

func (h *fakeLeaseHandle) Key() string                     { return h.key }
func (h *fakeLeaseHandle) Owner() string                   { return h.owner }
func (h *fakeLeaseHandle) Release(_ context.Context) error { return nil }

type fakeFailoverHandler struct {
	called bool
	err    error
}

func (h *fakeFailoverHandler) HandleFailover(_ context.Context, _ domain.ClusterSpec, _ *domain.ClusterView) error {
	h.called = true
	return h.err
}

// --- helpers ---

func testSpec() domain.ClusterSpec {
	return domain.ClusterSpec{
		Name: "test",
		Controller: domain.ControllerSpec{
			ID: "manager-1",
			Lease: domain.LeaseSpec{
				Backend: "local-memory",
				TTL:     15 * time.Second,
			},
			Monitor: domain.MonitorSpec{
				Interval:         10 * time.Millisecond,
				FailureThreshold: 2,
				ReconfirmTimeout: 50 * time.Millisecond,
			},
		},
		Topology: domain.TopologySpec{Kind: domain.TopologyMySQLReplicationSinglePrimary},
		Replication: domain.ReplicationSpec{
			Mode:     domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{Policy: domain.SemiSyncPreferred},
			Salvage:  domain.SalvageSpec{Policy: domain.SalvageIfPossible},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", Host: "10.0.0.1", Port: 3306, VersionSeries: "8.4", ExpectedRole: domain.NodeRolePrimary},
			{ID: "db2", Host: "10.0.0.2", Port: 3306, VersionSeries: "8.4", ExpectedRole: domain.NodeRoleReplica},
		},
	}
}

func newTestEngine(disc topology.Discoverer) *Engine {
	store := state.NewMemoryStore()
	logger := obs.NewLogger("error") // suppress logs in tests
	return NewEngine(disc, store, logger)
}

// --- unit tests for the step() state machine ---

// stubProbe lets tests replace the ping and secondary-check callables.
// We test step() directly using the exported-for-test variant.

func TestStepHealthyStaysHealthyOnSuccess(t *testing.T) {
	disc := &fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	}
	e := newTestEngine(disc)
	e.probePrimary = func(_ context.Context, ns domain.NodeSpec) error { return nil }

	spec := testSpec()
	view := singleView(domain.NodeHealthAlive)
	phase, count, _ := e.step(context.Background(), spec, view, phaseHealthy, 0, 2)

	if phase != phaseHealthy {
		t.Fatalf("expected healthy, got %s", phase)
	}
	if count != 0 {
		t.Fatalf("expected failureCount=0, got %d", count)
	}
}

func TestStepHealthyMovesSuspectOnFailure(t *testing.T) {
	e := newTestEngine(&fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	})
	e.probePrimary = func(_ context.Context, _ domain.NodeSpec) error { return errors.New("connection refused") }

	spec := testSpec()
	view := singleView(domain.NodeHealthAlive)
	phase, count, _ := e.step(context.Background(), spec, view, phaseHealthy, 0, 2)

	if phase != phaseSuspect {
		t.Fatalf("expected suspect, got %s", phase)
	}
	if count != 1 {
		t.Fatalf("expected failureCount=1, got %d", count)
	}
}

func TestStepSuspectMovesToSecondaryCheckAtThreshold(t *testing.T) {
	e := newTestEngine(&fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	})
	e.probePrimary = func(_ context.Context, _ domain.NodeSpec) error { return errors.New("timeout") }
	// secondary check returns false (all replicas IO thread not running)
	e.runSecondaryChecksFunc = func(_ context.Context, _ domain.ClusterSpec, _ *domain.ClusterView) bool { return false }

	spec := testSpec()
	view := singleView(domain.NodeHealthAlive)
	// failureCount=1 already, threshold=2 → this probe makes it 2 → threshold hit
	phase, count, _ := e.step(context.Background(), spec, view, phaseSuspect, 1, 2)

	if phase != phaseSecondaryCheck {
		t.Fatalf("expected secondary-check, got %s", phase)
	}
	if count != 2 {
		t.Fatalf("expected failureCount=2, got %d", count)
	}
}

func TestStepSecondaryCheckResetsToHealthyWhenAlive(t *testing.T) {
	e := newTestEngine(&fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	})
	e.runSecondaryChecksFunc = func(_ context.Context, _ domain.ClusterSpec, _ *domain.ClusterView) bool { return true }

	spec := testSpec()
	view := singleView(domain.NodeHealthAlive)
	phase, count, _ := e.step(context.Background(), spec, view, phaseSecondaryCheck, 2, 2)

	if phase != phaseHealthy {
		t.Fatalf("expected healthy, got %s", phase)
	}
	if count != 0 {
		t.Fatalf("expected failureCount=0, got %d", count)
	}
}

func TestStepSecondaryCheckMovesToReconfirmWhenDead(t *testing.T) {
	e := newTestEngine(&fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	})
	e.runSecondaryChecksFunc = func(_ context.Context, _ domain.ClusterSpec, _ *domain.ClusterView) bool { return false }

	spec := testSpec()
	view := singleView(domain.NodeHealthAlive)
	phase, _, _ := e.step(context.Background(), spec, view, phaseSecondaryCheck, 2, 2)

	if phase != phaseReconfirmTopology {
		t.Fatalf("expected reconfirm-topology, got %s", phase)
	}
}

func TestStepReconfirmMovesToDeadConfirmedWhenPrimaryDead(t *testing.T) {
	deadView := singleView(domain.NodeHealthDead)
	deadView.Nodes[0].Health = domain.NodeHealthDead

	e := newTestEngine(&fakeDiscoverer{
		views: []*domain.ClusterView{deadView},
		errs:  []error{nil},
	})

	spec := testSpec()
	spec.Controller.Monitor.ReconfirmTimeout = 50 * time.Millisecond
	view := singleView(domain.NodeHealthAlive)
	phase, _, _ := e.step(context.Background(), spec, view, phaseReconfirmTopology, 2, 2)

	if phase != phaseDeadConfirmed {
		t.Fatalf("expected dead-confirmed, got %s", phase)
	}
}

func TestStepReconfirmResetsToHealthyWhenPrimaryAlive(t *testing.T) {
	e := newTestEngine(&fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	})

	spec := testSpec()
	spec.Controller.Monitor.ReconfirmTimeout = 50 * time.Millisecond
	view := singleView(domain.NodeHealthAlive)
	phase, count, _ := e.step(context.Background(), spec, view, phaseReconfirmTopology, 2, 2)

	if phase != phaseHealthy {
		t.Fatalf("expected healthy, got %s", phase)
	}
	if count != 0 {
		t.Fatalf("expected failureCount=0, got %d", count)
	}
}

// --- integration-style test for Run() ---

func TestRunTriggersFailoverWhenPrimaryDead(t *testing.T) {
	aliveView := singleView(domain.NodeHealthAlive)
	deadView := singleView(domain.NodeHealthDead)
	deadView.Nodes[0].Health = domain.NodeHealthDead

	// First call: initial discovery (alive).
	// Subsequent calls during reconfirm: dead.
	disc := &fakeDiscoverer{
		views: []*domain.ClusterView{aliveView, deadView, deadView},
		errs:  []error{nil, nil, nil},
	}

	store := state.NewMemoryStore()
	logger := obs.NewLogger("error")
	leases := &fakeLeases{}
	// inspector is unused because we inject probe stubs below
	engine := NewManagerEngine(disc, store, leases, nil, nil, logger)

	probeCount := 0
	engine.probePrimary = func(_ context.Context, _ domain.NodeSpec) error {
		probeCount++
		return errors.New("connection refused") // always fail
	}
	engine.runSecondaryChecksFunc = func(_ context.Context, _ domain.ClusterSpec, _ *domain.ClusterView) bool {
		return false // no secondary sees primary
	}

	handler := &fakeFailoverHandler{}
	spec := testSpec()
	spec.Controller.Monitor.Interval = 5 * time.Millisecond
	spec.Controller.Monitor.FailureThreshold = 2
	spec.Controller.Monitor.ReconfirmTimeout = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := engine.Run(ctx, spec, handler)
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if !handler.called {
		t.Fatal("expected HandleFailover to be called, but it was not")
	}
}

func TestRunReturnsContextCancelled(t *testing.T) {
	disc := &fakeDiscoverer{
		views: []*domain.ClusterView{singleView(domain.NodeHealthAlive)},
		errs:  []error{nil},
	}
	store := state.NewMemoryStore()
	logger := obs.NewLogger("error")
	leases := &fakeLeases{}
	engine := NewManagerEngine(disc, store, leases, nil, nil, logger)
	engine.probePrimary = func(_ context.Context, _ domain.NodeSpec) error { return nil }

	spec := testSpec()
	spec.Controller.Monitor.Interval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	err := engine.Run(ctx, spec, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
