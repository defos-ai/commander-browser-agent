package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Base64 framing keeps this raw-byte allowance below the private 1 MiB frame
// limit. The local process transport retains its separate larger HTTP bounds.
const modelRelayLimit = 512 << 10

type modelRelayRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Token  string `json:"token"`
	Body   []byte `json:"body"`
}

type modelRelayResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
}

// beginRelay must run before the owner starts its RPC goroutine. The matching
// endRelay belongs after the response write, so Close also joins delivery work.
// Completed IDs remain retired for this bounded execution and cannot be replayed.
func (m *modelTransport) beginRelay(identifier string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, seen := m.relayIDs[identifier]
	if !m.relayOnly || m.closing || m.ctx.Err() != nil || m.activeRelayID != "" || m.relayCalls >= m.limit+8 ||
		identifier == "" || len(identifier) > 128 || !utf8.ValidString(identifier) || strings.ContainsRune(identifier, 0) || seen {
		return false
	}
	if m.relayIDs == nil {
		m.relayIDs = make(map[string]bool, m.limit+8)
	}
	m.relayIDs[identifier] = true
	m.activeRelayID = identifier
	m.relayCalls++
	m.relays.Add(1)
	return true
}

// finishRelayWork releases provider admission before response delivery. A fast
// next SDK call can arrive while the previous sender finishes its bookkeeping.
// The delivery reservation remains live until endRelay, so Close still joins it.
func (m *modelTransport) finishRelayWork(identifier string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeRelayID != identifier {
		m.fault = true
		return
	}
	m.activeRelayID = ""
}

func (m *modelTransport) endRelay(identifier string) {
	m.mu.Lock()
	if !m.relayIDs[identifier] {
		m.fault = true
		m.mu.Unlock()
		return
	}
	m.relayIDs[identifier] = false
	if m.activeRelayID == identifier {
		m.activeRelayID = ""
	}
	m.mu.Unlock()
	m.relays.Done()
}

// relay deliberately reuses handle's credential, JSON-protection, exact-model,
// response-allowance and forwarding policy. No worker header or endpoint can
// reach the provider; even SDK metadata traverses the same protection boundary.
// Its caller must own a beginRelay reservation and clear the returned body.
func (m *modelTransport) relay(ctx context.Context, message modelRelayRequest) modelRelayResponse {
	m.mu.Lock()
	allowed := m.relayOnly && m.activeRelayID != "" && !m.closing && m.ctx.Err() == nil
	m.mu.Unlock()
	if !allowed || ctx.Err() != nil || len(message.Body) > modelRelayLimit || len(message.Token) != 64 ||
		!(message.Method == http.MethodGet && message.Path == "/v1/models" && len(message.Body) == 0 ||
			message.Method == http.MethodPost && message.Path == "/v1/chat/completions" && len(message.Body) > 0) {
		return modelRelayFailure()
	}
	active, cancel := context.WithCancel(ctx)
	watch := context.AfterFunc(m.ctx, cancel)
	defer func() { watch(); cancel() }()
	request, err := http.NewRequestWithContext(active, message.Method, "http://private-model"+message.Path,
		io.NopCloser(bytes.NewReader(message.Body)))
	if err != nil {
		return modelRelayFailure()
	}
	request.Header.Set("Authorization", "Bearer "+message.Token)
	writer := relayModelWriter{header: make(http.Header)}
	m.handle(&writer, request)
	if writer.failed || active.Err() != nil || writer.status != http.StatusOK && writer.status != http.StatusBadRequest ||
		writer.header.Get("Content-Type") != "application/json" || len(writer.body) == 0 || !json.Valid(writer.body) {
		clear(writer.body)
		return modelRelayFailure()
	}
	return modelRelayResponse{Status: writer.status, Body: writer.body}
}

func modelRelayFailure() modelRelayResponse {
	return modelRelayResponse{Status: http.StatusBadRequest, Body: []byte(modelFailureJSON)}
}

// relayModelWriter is an internal bounded sink, not an HTTP server or an
// unbounded recorder. Oversized responses fail without returning a prefix.
type relayModelWriter struct {
	header http.Header
	status int
	body   []byte
	failed bool
}

func (writer *relayModelWriter) Header() http.Header { return writer.header }

func (writer *relayModelWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	if status != http.StatusOK && status != http.StatusBadRequest {
		writer.failed = true
	}
}

func (writer *relayModelWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.failed || len(data) > modelRelayLimit-len(writer.body) {
		clear(writer.body)
		writer.body = nil
		writer.failed = true
		return 0, errModelTransport
	}
	writer.body = append(writer.body, data...)
	return len(data), nil
}
