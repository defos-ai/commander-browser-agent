package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/defos-ai/commander-browser-agent/internal/hermes/containers"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander-browser-agent/internal/textrender"
	"github.com/defos-ai/commander/framework/agent/action"
)

// ProtectText applies both the callback's manager-owned protection and this
// worker's retained fingerprints before the caller truncates a source excerpt.
func (browser *Browser) ProtectText(ctx context.Context, text string) (string, error) {
	release, err := browser.borrow(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return browser.protectText(text)
}

func (browser *Browser) protectText(text string) (string, error) {
	if len(text) > maxBrowserResult || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", ErrProtocol
	}
	protected := sensitive.Redact(browser.owner, text)
	protected = sensitive.Redact(sensitive.WithValues(browser.owner, browser.worker.values), protected)
	if err := contextError(browser.lease); err != nil {
		return "", errors.Join(ErrClosed, err)
	}
	return protected, nil
}

// RenderText lends only protected plain text to an isolated, unauthenticated
// renderer. The borrow lasts through its allocator/socket cleanup receipt.
func (browser *Browser) RenderText(ctx context.Context, text string, width, height int) ([]byte, error) {
	release, err := browser.borrow(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	text, err = browser.protectText(text)
	if err != nil {
		return nil, err
	}
	if err := textrender.Validate(text, width, height); err != nil {
		return nil, err
	}
	reply, err := browser.request(ctx, browserCommand{Command: "renderer"})
	if err != nil {
		return nil, err
	}
	defer clear(reply)
	var result struct {
		Endpoint string `json:"endpoint"`
	}
	if strictJSON(reply, &result) != nil {
		browser.worker.fail(ErrProtocol)
		return nil, ErrProtocol
	}
	endpoint, err := rendererEndpoint(result.Endpoint)
	if err != nil {
		browser.worker.fail(ErrProtocol)
		return nil, ErrProtocol
	}
	var png []byte
	if browser.worker.container == nil {
		png, err = browser.worker.render.Render(ctx, browser.lease, endpoint, text, width, height)
	} else {
		png, err = browser.worker.render.RenderUsing(ctx, browser.lease, text, width, height, func(active context.Context) ([]byte, error) {
			if err := action.Log(active, "Rendering the protected screenshot"); err != nil {
				return nil, errors.Join(ErrUnavailable, contextError(active))
			}
			png, err := browser.worker.container.Render(active, endpoint, text, width, height)
			if errors.Is(err, containers.ErrUncertain) {
				browser.mu.Lock()
				browser.uncertain = true
				browser.mu.Unlock()
				browser.worker.fail(ErrUncertain)
			}
			return png, err
		})
		if err == nil {
			// Reconfirm the original source before lending bytes to the caller.
			// The container's loopback endpoint is never dialed by this host.
			var check json.RawMessage
			check, err = browser.request(ctx, browserCommand{Command: "renderer"})
			var confirmed struct {
				Endpoint string `json:"endpoint"`
			}
			if err == nil && (strictJSON(check, &confirmed) != nil || confirmed.Endpoint != result.Endpoint) {
				browser.worker.fail(ErrProtocol)
				err = ErrProtocol
			}
			clear(check)
		}
	}
	if ctx.Err() != nil || browser.lease.Err() != nil {
		clear(png)
		return nil, errors.Join(err, ErrClosed, contextError(ctx), contextError(browser.lease))
	}
	if err != nil {
		clear(png)
		return nil, err
	}
	return png, err
}

// The pinned Lightpanda server publishes an HTTP discovery root. Its CDP socket
// uses the same root and port; this is deliberately not a general URL resolver.
func rendererEndpoint(raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || endpoint.Scheme != "http" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || strings.Contains(raw, "#") ||
		endpoint.Opaque != "" || endpoint.RawPath != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return "", ErrProtocol
	}
	ip := net.ParseIP(endpoint.Hostname())
	port, err := strconv.Atoi(endpoint.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return "", ErrProtocol
	}
	endpoint.Scheme = "ws"
	return endpoint.String(), nil
}
