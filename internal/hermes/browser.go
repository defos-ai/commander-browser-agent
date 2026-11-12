package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const maxBrowserMessage = 32 << 10

// BrowserEvent is a private, filtered event from the pinned daemon connection.
// It never contains request headers, cookies or POST data.
type BrowserEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type browserCommand struct {
	Lease     string          `json:"lease"`
	Command   string          `json:"command"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	TimeoutMS int             `json:"timeout_ms,omitempty"`
	Uncertain bool            `json:"uncertain,omitempty"`
}

type browserReply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	State  string          `json:"state,omitempty"`
}

// Browser lends trusted Go code the retained native connection. It is not a
// model tool. Its authority ends with the WithBrowser callback; cancellation
// does not revoke the owner's bounded cleanup path while that callback unwinds.
type Browser struct {
	worker               *Worker
	id                   string
	owner                context.Context
	mu                   sync.Mutex
	active               bool
	uncertain            bool
	loadingStopped       bool
	interceptionDisabled bool
	uses                 sync.WaitGroup
}

// WithBrowser serializes deterministic host work against Hermes reasoning.
// Browser construction can bootstrap an empty native page, never a model.
func WithBrowser(ctx context.Context, work func(*Browser) error) error {
	if work == nil {
		return ErrUnavailable
	}
	return withGrantedWorker(ctx, func(active context.Context, worker *Worker) (runErr error) {
		select {
		case worker.gate <- struct{}{}:
			defer func() { <-worker.gate }()
		case <-active.Done():
			return contextError(active)
		case <-worker.Done():
			return ErrClosed
		}
		if err := contextError(active); err != nil {
			return err
		}
		identifier, err := freshID()
		if err != nil {
			return err
		}
		owner, stop := context.WithCancelCause(context.WithoutCancel(active))
		watch := context.AfterFunc(worker.lifetime, func() { stop(context.Cause(worker.lifetime)) })
		defer watch()
		defer stop(ErrClosed)
		browser := &Browser{worker: worker, id: identifier, owner: owner, active: true}
		worker.mu.Lock()
		if worker.closing || worker.fault || !worker.ready || worker.browser != nil {
			worker.mu.Unlock()
			return ErrClosed
		}
		worker.browser = browser
		worker.mu.Unlock()
		started, returned := false, false
		defer func() {
			browser.mu.Lock()
			browser.active = false
			if started && !returned {
				browser.uncertain = true
			}
			browser.mu.Unlock()
			browser.uses.Wait()
			if started {
				browser.mu.Lock()
				uncertain := browser.uncertain
				browser.mu.Unlock()
				cleanup, cancel := context.WithTimeout(owner, 4*time.Second)
				_, finishErr := browser.request(cleanup, browserCommand{Command: "end", Uncertain: uncertain})
				cancel()
				if finishErr != nil {
					worker.mu.Lock()
					closing := worker.closing
					worker.mu.Unlock()
					if !closing {
						worker.fail(ErrUncertain)
					}
					runErr = errors.Join(runErr, finishErr)
				}
			}
			worker.mu.Lock()
			if worker.browser == browser {
				worker.browser = nil
			}
			worker.mu.Unlock()
		}()
		if _, err := browser.request(active, browserCommand{Command: "begin"}); err != nil {
			browser.mu.Lock()
			browser.active = false
			browser.mu.Unlock()
			worker.mu.Lock()
			closing := worker.closing
			worker.mu.Unlock()
			if !closing {
				worker.fail(ErrUncertain)
			}
			return errors.Join(err, contextError(active))
		}
		started = true
		err = work(browser)
		returned = true
		return errors.Join(err, contextError(active))
	})
}

func (browser *Browser) use(ctx context.Context, command browserCommand) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	browser.mu.Lock()
	if !browser.active || browser.owner.Err() != nil {
		browser.mu.Unlock()
		return nil, ErrClosed
	}
	browser.uses.Add(1)
	browser.mu.Unlock()
	defer browser.uses.Done()
	return browser.request(ctx, command)
}

func (browser *Browser) request(ctx context.Context, command browserCommand) (json.RawMessage, error) {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	watch := context.AfterFunc(browser.owner, cancel)
	defer watch()
	if err := contextError(bounded); err != nil {
		return nil, err
	}
	identifier, err := freshID()
	if err != nil {
		return nil, err
	}
	identifier = browser.id + ":" + identifier
	command.Lease = browser.id
	deadline, _ := bounded.Deadline()
	command.TimeoutMS = max(1, min(8000, int(time.Until(deadline)/time.Millisecond)))
	response := make(chan browserReply, 1)
	worker := browser.worker
	worker.mu.Lock()
	if worker.closing || worker.fault || !worker.ready || worker.browser != browser || len(worker.browserCalls) >= 64 {
		worker.mu.Unlock()
		return nil, ErrClosed
	}
	worker.browserCalls[identifier] = response
	worker.mu.Unlock()
	defer func() {
		worker.mu.Lock()
		if _, exists := worker.browserCalls[identifier]; exists {
			worker.browserCalls[identifier] = nil // Late replies remain correlated.
		}
		worker.mu.Unlock()
	}()
	if err := worker.send(bounded, "browser", identifier, command); err != nil {
		if errors.Is(err, ErrProtocol) {
			worker.fail(ErrProtocol)
		} else {
			worker.mu.Lock()
			delete(worker.browserCalls, identifier) // No command bytes were written.
			worker.mu.Unlock()
		}
		return nil, err
	}
	select {
	case reply := <-response:
		if reply.State == "lost" {
			worker.fail(ErrLost)
			return nil, ErrLost
		}
		if !reply.OK {
			browser.mu.Lock()
			browser.uncertain = true
			browser.mu.Unlock()
			return nil, ErrUncertain
		}
		if err := contextError(bounded); err != nil {
			return nil, err
		}
		return reply.Result, nil
	case <-bounded.Done():
		browser.mu.Lock()
		if command.Command != "events" || !browser.loadingStopped || !browser.interceptionDisabled {
			browser.uncertain = true
		}
		browser.mu.Unlock()
		return nil, contextError(bounded)
	}
}

// Execute accepts host-generated CDP parameters only. The worker fixes routing
// to its original attachment and never interprets these as model instructions.
func (browser *Browser) Execute(ctx context.Context, method string, params any, result any) error {
	switch method {
	case "Page.enable", "Page.navigate", "Page.getFrameTree", "Page.createIsolatedWorld", "Page.stopLoading",
		"Runtime.enable", "Runtime.evaluate", "Runtime.callFunctionOn", "Network.enable",
		"Fetch.enable", "Fetch.disable", "Fetch.continueRequest", "Fetch.failRequest":
	default:
		return ErrProtocol
	}
	encoded, err := json.Marshal(params)
	if err != nil || len(encoded) > maxBrowserMessage {
		clear(encoded)
		return ErrProtocol
	}
	defer clear(encoded)
	reply, err := browser.use(ctx, browserCommand{Command: "call", Method: method, Params: encoded})
	if err != nil {
		return err
	}
	defer clear(reply)
	if result != nil && json.Unmarshal(reply, result) != nil {
		browser.mu.Lock()
		browser.uncertain = true
		browser.mu.Unlock()
		return ErrProtocol
	}
	browser.mu.Lock()
	switch method {
	case "Fetch.enable":
		browser.loadingStopped, browser.interceptionDisabled = false, false
	case "Page.stopLoading":
		browser.loadingStopped = true
	case "Fetch.disable":
		browser.interceptionDisabled = true
	}
	browser.mu.Unlock()
	return nil
}

func (browser *Browser) Drain(ctx context.Context) ([]BrowserEvent, error) {
	reply, err := browser.use(ctx, browserCommand{Command: "events"})
	if err != nil {
		return nil, err
	}
	defer clear(reply)
	var events []BrowserEvent
	if json.Unmarshal(reply, &events) != nil || len(events) > 128 {
		browser.mu.Lock()
		browser.uncertain = true
		browser.mu.Unlock()
		return nil, ErrProtocol
	}
	for _, event := range events {
		switch event.Method {
		case "Fetch.requestPaused", "Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed":
		default:
			browser.mu.Lock()
			browser.uncertain = true
			browser.mu.Unlock()
			return nil, ErrProtocol
		}
	}
	return events, nil
}

func (browser *Browser) CloseTarget(ctx context.Context) error {
	_, err := browser.use(ctx, browserCommand{Command: "destroy"})
	return err
}
