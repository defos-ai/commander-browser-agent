package textrender

import "context"

// Progress reports the fixed rendering milestone for this execution. It accepts
// no browser content and must respect the supplied renderer lifetime.
type Progress func(context.Context) error

type progressKey struct{}

// WithProgress binds progress to the current execution, never the retained owner.
// A nil callback removes inherited progress access.
func WithProgress(ctx context.Context, progress Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, progress)
}

func reportProgress(ctx context.Context) error {
	progress, _ := ctx.Value(progressKey{}).(Progress)
	if progress == nil {
		return errRenderer
	}
	return progress(ctx)
}
