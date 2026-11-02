package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

const browserSessionOwner = "browser-agent-0123abcd"

func TestBrowserSessionsPrepareClaimsWithoutStartingResources(t *testing.T) {
	manager := newSessionManagerFixture(t)
	claim := browserSessionClaim()
	if err := manager.Prepare(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	initial := manager.Snapshot()
	if len(initial) != 1 || initial[0].Reference != claim.Reference || initial[0].State != "starting" || initial[0].Busy || initial[0].Endpoint != "" || initial[0].VerifiedAt != nil {
		t.Fatalf("unexpected prepared inventory: %+v", initial)
	}
	if err := initial[0].Validate(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.Changes():
	default:
		t.Fatal("preparation did not notify inventory")
	}
	if err := manager.Prepare(t.Context(), claim); err != nil || manager.Snapshot()[0].Revision != initial[0].Revision {
		t.Fatalf("matching preparation was not idempotent: %v", err)
	}
	initial[0].Label = "caller mutation"
	if manager.Snapshot()[0].Label == initial[0].Label {
		t.Fatal("snapshot shares mutable provider metadata")
	}
	inspect, err := manager.Control(t.Context(), sessions.Control{Reference: claim.Reference, Method: "inspect"})
	if err != nil || inspect.Revision != initial[0].Revision || inspect.State != "starting" {
		t.Fatalf("inspection changed an unstarted claim: %+v, %v", inspect, err)
	}
	if _, err := manager.Control(t.Context(), sessions.Control{Reference: claim.Reference, Method: "verify"}); err == nil {
		t.Fatal("verification started an unstarted resource")
	}
	select {
	case <-manager.Changes():
		t.Fatal("read-only inspection or duplicate preparation changed inventory")
	default:
	}
}

func TestBrowserSessionsRejectOwnershipAndClaimCollisions(t *testing.T) {
	for _, owner := range []string{"", "browser-agent", "browser-agent-0123ABCD"} {
		if _, err := browserruntime.New(owner); err == nil {
			t.Fatalf("accepted invalid owner %q", owner)
		}
	}
	manager := newSessionManagerFixture(t)
	claim := browserSessionClaim()
	if err := manager.Prepare(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	other := claim
	other.OwnerAgentID = "browser-agent-89abcdef"
	if err := manager.Prepare(t.Context(), other); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("accepted another owner: %v", err)
	}
	other = claim
	other.Creation.TaskID = id.New()
	other.Creation.CommandID = other.Creation.TaskID
	if err := manager.Prepare(t.Context(), other); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("accepted another creation for a retained ID: %v", err)
	}
	other = claim
	other.ScopeID = id.New()
	if err := manager.Prepare(t.Context(), other); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("accepted another scope for a retained ID: %v", err)
	}
	other = browserSessionClaim()
	other.ScopeID = claim.ScopeID
	if err := manager.Prepare(t.Context(), other); err != nil {
		t.Fatalf("rejected an independently admitted claim in the same scope: %v", err)
	}
	if len(manager.Snapshot()) != 2 {
		t.Fatal("independent claim replaced a retained identity")
	}
}

func TestBrowserSessionsCloseBeforePrepareRetainsTombstone(t *testing.T) {
	manager := newSessionManagerFixture(t)
	claim := browserSessionClaim()
	request := sessions.Control{Reference: claim.Reference, Method: "close", RequestID: id.New()}
	if _, err := manager.Control(t.Context(), request); err == nil || len(manager.Snapshot()) != 0 {
		t.Fatalf("unknown close without creation changed inventory: %v", err)
	}
	request.Creation = &claim.Creation
	closed, err := manager.Control(t.Context(), request)
	if err != nil || closed.Reference != claim.Reference || closed.State != "closed" || closed.Busy {
		t.Fatalf("close did not tombstone the unstarted identity: %+v, %v", closed, err)
	}
	if err := manager.Prepare(t.Context(), claim); !errors.Is(err, sessions.ErrSessionClosed) {
		t.Fatalf("late preparation resurrected a closed session: %v", err)
	}
	repeated, err := manager.Control(t.Context(), request)
	if err != nil || repeated.Revision != closed.Revision {
		t.Fatalf("duplicate close changed terminal evidence: %+v, %v", repeated, err)
	}
	replacement := browserSessionClaim()
	replacement.ScopeID = claim.ScopeID
	if err := manager.Prepare(t.Context(), replacement); err != nil {
		t.Fatalf("fresh framework claim was rejected: %v", err)
	}
	if len(manager.Snapshot()) != 2 {
		t.Fatal("replacement discarded the retained closed identity")
	}
}

func TestBrowserSessionsRequirePreparedFrameworkBinding(t *testing.T) {
	manager := newSessionManagerFixture(t)
	claim := browserSessionClaim()
	options := browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: filepath.Join(t.TempDir(), "never-launched")}
	work := func(context.Context) error { t.Error("work ran without a usable prepared binding"); return nil }
	if _, err := manager.Run(t.Context(), options, work); err == nil {
		t.Fatal("accepted work without a framework binding")
	}
	binding := sessions.Binding{Reference: claim.Reference, Creation: claim.Creation, Mode: sessions.Create}
	ctx := action.WithSession(t.Context(), &binding)
	if _, err := manager.Run(ctx, options, work); !errors.Is(err, sessions.ErrUnknownSession) {
		t.Fatalf("implicitly prepared an unknown session: %v", err)
	}
	if err := manager.Prepare(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	wrong := binding
	wrong.Creation.OperationID = id.New()
	if _, err := manager.Run(action.WithSession(t.Context(), &wrong), options, work); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("accepted a different creation binding: %v", err)
	}
	wrong = binding
	wrong.Mode = sessions.Use
	if _, err := manager.Run(action.WithSession(t.Context(), &wrong), options, work); err == nil {
		t.Fatal("use implicitly created a resource")
	}
	request := sessions.Control{Reference: claim.Reference, Creation: &claim.Creation, Method: "close", RequestID: id.New()}
	if _, err := manager.Control(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(ctx, options, work); !errors.Is(err, sessions.ErrSessionClosed) {
		t.Fatalf("work resurrected an explicitly closed identity: %v", err)
	}
}

func TestBrowserSessionsDelayedCloseDoesNotAffectOtherIdentity(t *testing.T) {
	manager := newSessionManagerFixture(t)
	current := browserSessionClaim()
	if err := manager.Prepare(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	old := browserSessionClaim()
	old.ScopeID = current.ScopeID
	request := sessions.Control{Reference: old.Reference, Creation: &old.Creation, Method: "close", RequestID: id.New()}
	closed, err := manager.Control(t.Context(), request)
	if err != nil || closed.State != "closed" || closed.Reference != old.Reference {
		t.Fatalf("delayed close was not tombstoned: %+v, %v", closed, err)
	}
	view, err := manager.Control(t.Context(), sessions.Control{Reference: current.Reference, Method: "inspect"})
	if err != nil || view.State != "starting" || view.Revision != 1 {
		t.Fatalf("delayed close changed the current owner: %+v, %v", view, err)
	}
	if err := manager.Prepare(t.Context(), old); !errors.Is(err, sessions.ErrSessionClosed) {
		t.Fatalf("late creation resurrected the old identity: %v", err)
	}
	replacement := browserSessionClaim()
	replacement.ScopeID = current.ScopeID
	if err := manager.Prepare(t.Context(), replacement); err != nil {
		t.Fatalf("rejected a separately admitted replacement: %v", err)
	}
	if items := manager.Snapshot(); len(items) != 3 {
		t.Fatalf("new claim discarded retained identities: %+v", items)
	} else {
		for _, item := range items {
			want := "starting"
			if item.ID == old.ID {
				want = "closed"
			}
			if item.State != want {
				t.Fatalf("another identity's close changed state: %+v", item)
			}
		}
	}
}

func TestBrowserSessionsRequireExplicitLightpandaEngine(t *testing.T) {
	for _, engine := range []browserruntime.Engine{"", "chromium", "automatic"} {
		t.Run(string(engine), func(t *testing.T) {
			manager := newSessionManagerFixture(t)
			claim := browserSessionClaim()
			if err := manager.Prepare(t.Context(), claim); err != nil {
				t.Fatal(err)
			}
			binding := sessions.Binding{Reference: claim.Reference, Creation: claim.Creation, Mode: sessions.Create}
			options := browserruntime.Options{Engine: engine, Executable: filepath.Join(t.TempDir(), "never-launched")}
			_, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(context.Context) error {
				t.Error("work ran with an unsupported or implicit engine")
				return nil
			})
			if err == nil {
				t.Fatal("accepted an unsupported or implicit engine")
			}
			if view := manager.Snapshot()[0]; view.State != "starting" || view.Revision != 1 || view.Busy {
				t.Fatalf("invalid engine changed the unstarted identity: %+v", view)
			}
		})
	}
}

func TestBrowserSessionsShutdownRetainsLostAndClosedReferences(t *testing.T) {
	manager := newSessionManagerFixture(t)
	lost, closed := browserSessionClaim(), browserSessionClaim()
	if err := manager.Prepare(t.Context(), lost); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Control(t.Context(), sessions.Control{Reference: closed.Reference, Creation: &closed.Creation, Method: "close", RequestID: id.New()}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, item := range manager.Snapshot() {
		want := "closed"
		if item.ID == lost.ID {
			want = "lost"
		}
		if item.State != want || item.Busy {
			t.Fatalf("shutdown fabricated explicit closure: %+v", item)
		}
	}
	if err := manager.Prepare(t.Context(), browserSessionClaim()); err == nil {
		t.Fatal("stopped manager accepted a new claim")
	}
	view, err := manager.Control(t.Context(), sessions.Control{Reference: lost.Reference, Method: "inspect"})
	if err != nil || view.State != "lost" {
		t.Fatalf("shutdown lost its retained inventory: %+v, %v", view, err)
	}
}

func TestBrowserSessionsCanceledAndConcurrentControlsPreserveInventory(t *testing.T) {
	manager := newSessionManagerFixture(t)
	claim := browserSessionClaim()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.Prepare(ctx, claim); !errors.Is(err, context.Canceled) || len(manager.Snapshot()) != 0 {
		t.Fatalf("canceled preparation changed inventory: %v", err)
	}
	request := sessions.Control{Reference: claim.Reference, Creation: &claim.Creation, Method: "close", RequestID: id.New()}
	if _, err := manager.Control(ctx, request); !errors.Is(err, context.Canceled) || len(manager.Snapshot()) != 0 {
		t.Fatalf("canceled close changed inventory: %v", err)
	}
	if err := manager.Prepare(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 20 {
				items := manager.Snapshot()
				items[0].Label = "caller mutation"
				_, _ = manager.Control(t.Context(), sessions.Control{Reference: claim.Reference, Method: "inspect"})
			}
		})
	}
	if _, err := manager.Control(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if item := manager.Snapshot()[0]; item.State != "closed" || item.Label == "caller mutation" {
		t.Fatalf("concurrent readers changed retained state: %+v", item)
	}
}

func newSessionManagerFixture(t *testing.T) *browserruntime.Manager {
	t.Helper()
	manager, err := browserruntime.New(browserSessionOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close session manager: %v", err)
		}
	})
	return manager
}

func browserSessionClaim() sessions.CreationClaim {
	taskID := id.New()
	return sessions.CreationClaim{Reference: sessions.Reference{ID: id.New(), ScopeID: id.New(), OwnerAgentID: browserSessionOwner},
		Creation: sessions.ExecutionRef{OperationID: id.New(), TaskID: taskID, CommandID: taskID}}
}
