package browser

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
)

// HermesOptions selects the discovered local runtime paths.
// A session pins the complete selection for its lifetime.
// Lightpanda is the only supported browser engine.
type HermesOptions struct {
	Python     string
	Source     string
	Executable string
}

// Validate rejects incomplete discovered runtime selections before startup.
func (o HermesOptions) Validate() error {
	if !filepath.IsAbs(o.Python) || !filepath.IsAbs(o.Source) || !filepath.IsAbs(o.Executable) {
		return errors.New("Hermes requires absolute discovered runtime paths")
	}
	return nil
}

// StartHermes starts one native worker for an opaque session label. Its owner
// retains Commander identity and lifetime policy outside the runtime.
// A non-nil runtime on failure retains uncertain startup cleanup ownership.
func StartHermes(ctx context.Context, options HermesOptions, sessionID string) (Runtime, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	worker, err := hermes.Start(ctx, hermes.Config{
		Python: options.Python, Source: options.Source, Lightpanda: options.Executable, SessionID: sessionID,
	})
	if worker == nil {
		return nil, err
	}
	return &hermesRuntime{worker: worker}, err
}

type hermesRuntime struct {
	worker *hermes.Worker
}

var _ Runtime = (*hermesRuntime)(nil)

func (*hermesRuntime) String() string   { return "private Hermes runtime" }
func (*hermesRuntime) GoString() string { return "private Hermes runtime" }

func (r *hermesRuntime) Done() <-chan struct{} { return r.worker.Done() }
func (r *hermesRuntime) Verify(ctx context.Context) error {
	return r.worker.Verify(ctx)
}
func (r *hermesRuntime) Close() error { return r.worker.Close() }

// WithContext delegates to the existing callback grant and its cancellation
// joins. It adds neither a second execution gate nor a longer-lived capability.
func (r *hermesRuntime) WithContext(ctx context.Context, work func(context.Context) error) error {
	return hermes.WithWorker(ctx, r.worker, work)
}
