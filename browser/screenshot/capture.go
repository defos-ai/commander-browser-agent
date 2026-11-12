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

	"github.com/chromedp/cdproto/cdp"
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

type textSnapshot struct {
	Title     string `json:"title"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

type protector func(context.Context, string) (string, error)
type renderer func(context.Context, string, int, int) ([]byte, error)

var errCapture = errors.New("protected browser text snapshot could not be captured safely")

// Capture only reads the retained page. Native rasterization receives protected
// text in an isolated renderer connection and never sees the authenticated DOM.
func Capture(ctx context.Context, width, height int) (Image, error) {
	connection := chromedp.FromContext(ctx)
	if connection == nil || connection.Target == nil || connection.Browser == nil {
		return Image{}, captureError(ctx)
	}
	return capture(ctx, width, height, connection.Target, func(ctx context.Context, text string) (string, error) {
		return sensitive.Redact(ctx, text), nil
	}, browser.RenderText)
}

func capture(ctx context.Context, width, height int, executor cdp.Executor, protect protector, render renderer) (Image, error) {
	if ctx.Err() != nil {
		return Image{}, captureError(ctx)
	}
	if width <= 0 || height <= 0 || width > maxPixels/height {
		return Image{}, errors.New("screenshot dimensions exceed the supported pixel limit")
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	source, err := readSnapshot(bounded, executor)
	if err != nil {
		return Image{}, captureError(bounded)
	}
	truncated := source.Truncated || len(source.Title) > titleLimit || len(source.Text) > textLimit
	// Protect the complete bounded overlap before either excerpt is shortened.
	// Native sessions also apply their worker-owned fingerprints here.
	title, err := protect(bounded, source.Title)
	if err != nil {
		return Image{}, captureError(bounded)
	}
	text, err := protect(bounded, source.Text)
	if err != nil {
		return Image{}, captureError(bounded)
	}
	title = sensitive.Excerpt(bounded, title, titleLimit)
	text = sensitive.Excerpt(bounded, text, textLimit)
	protected := strings.TrimSpace(title + "\n\n" + text)
	data, err := render(bounded, protected, width, height)
	if err != nil {
		clear(data)
		return Image{}, captureError(bounded)
	}
	if len(data) > imageLimit || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		clear(data)
		return Image{}, errCapture
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != width || config.Height != height {
		clear(data)
		return Image{}, errCapture
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		clear(data)
		return Image{}, errCapture
	}
	if bounded.Err() != nil {
		clear(data)
		return Image{}, captureError(bounded)
	}
	return Image{PNG: data, Truncated: truncated}, nil
}

func readSnapshot(ctx context.Context, executor cdp.Executor) (textSnapshot, error) {
	ctx = cdp.WithExecutor(ctx, executor)
	tree, err := page.GetFrameTree().Do(ctx)
	if err != nil || tree == nil || tree.Frame == nil {
		return textSnapshot{}, errCapture
	}
	address, err := url.Parse(tree.Frame.URL)
	if err != nil || address.User != nil || address.Hostname() == "" || address.Opaque != "" || (address.Scheme != "http" && address.Scheme != "https") {
		return textSnapshot{}, errCapture
	}
	world, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("commander-text-snapshot").Do(ctx)
	if err != nil {
		return textSnapshot{}, errCapture
	}
	limits, _ := json.Marshal(map[string]int{"title": titleLimit + sensitive.MaxValueBytes - 1, "text": textLimit + sensitive.MaxValueBytes - 1})
	value, exception, err := cdpruntime.CallFunctionOn(snapshotDOM).WithExecutionContextID(world).
		WithArguments([]*cdpruntime.CallArgument{{Value: limits}}).WithSilent(true).WithReturnByValue(true).Do(ctx)
	var source textSnapshot
	if err != nil || exception != nil || value == nil || json.Unmarshal(value.Value, &source) != nil {
		return textSnapshot{}, errCapture
	}
	return source, nil
}

func captureError(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), context.Cause(ctx))
	}
	return errCapture
}
