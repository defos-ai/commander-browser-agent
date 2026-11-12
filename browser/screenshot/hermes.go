package screenshot

import (
	"context"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
)

// CaptureHermes reads an existing native session without an agent invocation.
// Its temporary renderer receives only protected text, never source DOM or state.
func CaptureHermes(ctx context.Context, width, height int) (image Image, captureErr error) {
	if _, err := action.UseSession(ctx); err != nil {
		return Image{}, err
	}
	if !sensitive.HasValues(ctx) {
		return Image{}, errCapture
	}
	captureErr = hermes.WithExistingBrowser(ctx, func(source *hermes.Browser) error {
		var err error
		image, err = capture(ctx, width, height, source, source.ProtectText, source.RenderText)
		return err
	})
	if captureErr != nil {
		clear(image.PNG)
		return Image{}, captureError(ctx)
	}
	return image, nil
}
