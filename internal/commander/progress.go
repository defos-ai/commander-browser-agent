package commander

import (
	"context"

	"github.com/defos-ai/commander-browser-agent/internal/textrender"
	"github.com/defos-ai/commander/framework/agent/action"
)

func withRenderProgress(ctx context.Context) context.Context {
	return textrender.WithProgress(ctx, func(progressCtx context.Context) error {
		return action.Log(progressCtx, "Rendering the protected screenshot")
	})
}
