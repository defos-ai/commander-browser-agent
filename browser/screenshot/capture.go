// Package screenshot renders a protected text snapshot of a retained browser page.
package screenshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
)

const Renderer = "lightpanda-text-snapshot"

const (
	titleLimit = 256
	textLimit  = 10000
	imageLimit = 8 << 20
	maxPixels  = 4_000_000
)

type Image struct {
	PNG       []byte
	Truncated bool
}

var errCapture = errors.New("protected browser text snapshot could not be captured safely")

// Capture only reads the retained page. Native rasterization receives protected
// text in an isolated renderer connection and never sees the authenticated DOM.
func Capture(ctx context.Context, width, height int) (Image, error) {
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	if width <= 0 || height <= 0 || width > maxPixels/height {
		return Image{}, errors.New("screenshot dimensions exceed the supported pixel limit")
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connection := chromedp.FromContext(bounded)
	if connection == nil || connection.Target == nil || connection.Browser == nil {
		return Image{}, errCapture
	}
	var source struct {
		Title     string `json:"title"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	err := chromedp.Run(bounded, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil || tree == nil || tree.Frame == nil {
			return errCapture
		}
		address, err := url.Parse(tree.Frame.URL)
		if err != nil || address.User != nil || address.Hostname() == "" || address.Opaque != "" || (address.Scheme != "http" && address.Scheme != "https") {
			return errCapture
		}
		world, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("commander-text-snapshot").Do(ctx)
		if err != nil {
			return errCapture
		}
		limits, _ := json.Marshal(map[string]int{"title": titleLimit + sensitive.MaxValueBytes - 1, "text": textLimit + sensitive.MaxValueBytes - 1})
		value, exception, err := cdpruntime.CallFunctionOn(snapshotDOM).WithExecutionContextID(world).
			WithArguments([]*cdpruntime.CallArgument{{Value: limits}}).WithSilent(true).WithReturnByValue(true).Do(ctx)
		if err != nil || exception != nil || value == nil || json.Unmarshal(value.Value, &source) != nil {
			return errCapture
		}
		return nil
	}))
	if err != nil {
		if bounded.Err() != nil {
			return Image{}, bounded.Err()
		}
		return Image{}, errCapture
	}
	truncated := source.Truncated || len(source.Title) > titleLimit || len(source.Text) > textLimit
	title := sensitive.Excerpt(bounded, source.Title, titleLimit)
	text := sensitive.Excerpt(bounded, source.Text, textLimit)
	protected := strings.TrimSpace(title + "\n\n" + text)
	data, err := browser.RenderText(bounded, protected, width, height)
	if err != nil {
		if bounded.Err() != nil {
			return Image{}, bounded.Err()
		}
		return Image{}, errCapture
	}
	if len(data) > imageLimit || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return Image{}, errCapture
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != width || config.Height != height {
		return Image{}, errCapture
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return Image{}, errCapture
	}
	if bounded.Err() != nil {
		return Image{}, bounded.Err()
	}
	return Image{PNG: data, Truncated: truncated}, nil
}
