package hermes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	modelRequestLimit  = 2 << 20
	modelResponseLimit = 1 << 20
	modelJSONDepth     = 64
)

var errModelTransport = errors.New("Hermes model transport is unavailable")

// modelTransport keeps the provider credential outside Hermes. Its local token
// authorizes one bounded execution against one fixed completion endpoint.
type modelTransport struct {
	baseURL string
	token   []byte
	key     []byte

	endpoint  string
	model     string
	limit     int
	maxTokens int
	redact    func(string) string
	ctx       context.Context
	cancel    context.CancelFunc
	server    *http.Server
	client    *http.Client
	http      *http.Transport
	served    chan struct{}
	joined    chan struct{}
	handlers  sync.WaitGroup
	mu        sync.Mutex
	closing   bool
	requests  int
	fault     bool
	close     sync.Once
	closeErr  error
}

func startModelTransport(ctx context.Context, request Request, redact func(string) string) (*modelTransport, error) {
	endpoint, err := url.Parse(request.Endpoint)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.ForceQuery || endpoint.Fragment != "" || strings.Contains(request.Endpoint, "#") || endpoint.Opaque != "" ||
		(endpoint.Scheme != "https" && (endpoint.Scheme != "http" ||
			!strings.EqualFold(endpoint.Hostname(), "localhost") && !net.ParseIP(endpoint.Hostname()).IsLoopback())) ||
		ctx.Err() != nil || redact == nil || len(request.APIKey) == 0 || len(request.APIKey) > 8192 ||
		!utf8.Valid(request.APIKey) || bytes.ContainsAny(request.APIKey, "\r\n\x00") ||
		request.Model == "" || request.MaxIterations < 1 || request.MaxIterations > 30 ||
		request.MaxTokens < 1 || request.MaxTokens > 16384 ||
		request.TimeoutSeconds < 1 || request.TimeoutSeconds > 600 {
		return nil, errModelTransport
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errModelTransport
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errModelTransport
	}
	timeout := time.Duration(request.TimeoutSeconds) * time.Second
	lifetime, cancel := context.WithTimeout(ctx, timeout)
	m := &modelTransport{baseURL: "http://" + listener.Addr().String() + "/v1",
		token: []byte(hex.EncodeToString(random[:])), key: bytes.Clone(request.APIKey),
		endpoint: request.Endpoint, model: request.Model, limit: request.MaxIterations + 1, maxTokens: request.MaxTokens,
		redact: redact, ctx: lifetime, cancel: cancel, served: make(chan struct{}), joined: make(chan struct{})}
	clear(random[:])
	// No environment proxies, redirects, automatic decompression,
	// shared connections or replayable request bodies belong to this transport.
	m.http = &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: timeout,
		DisableCompression: true, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10}
	m.client = &http.Client{Transport: m.http, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	m.server = &http.Server{Handler: http.HandlerFunc(m.handle), ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: timeout + 2*time.Second,
		IdleTimeout: 2 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return lifetime }}
	go func() {
		defer close(m.served)
		if err := m.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.mu.Lock()
			m.fault = true
			m.mu.Unlock()
		}
	}()
	return m, nil
}

func (m *modelTransport) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.closing || m.ctx.Err() != nil {
		m.mu.Unlock()
		modelFailure(w)
		return
	}
	m.handlers.Add(1)
	m.mu.Unlock()
	defer m.handlers.Done()
	defer r.Body.Close()
	authorization := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if authorization == r.Header.Get("Authorization") ||
		subtle.ConstantTimeCompare([]byte(authorization), m.token) != 1 || r.URL.RawQuery != "" || r.URL.ForceQuery {
		modelFailure(w)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		m.writeJSON(w, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": m.model, "object": "model", "created": 0, "owned_by": "commander"}}})
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, modelRequestLimit+1))
	defer clear(raw)
	if err != nil || len(raw) > modelRequestLimit || !uniqueModelRequestFields(raw) {
		modelFailure(w)
		return
	}
	value, err := m.cleanJSON(raw, modelRequestLimit)
	request, ok := value.(map[string]any)
	stream, hasStream := request["stream"]
	if err != nil || !ok || request["model"] != m.model || hasStream && stream != false || !m.constrainResponseAllowance(request) {
		modelFailure(w)
		return
	}
	request["stream"] = false
	body, err := json.Marshal(request)
	defer clear(body)
	if err != nil || len(body) > modelRequestLimit {
		modelFailure(w)
		return
	}
	m.mu.Lock()
	allowed := !m.closing && m.ctx.Err() == nil && m.requests < m.limit
	if allowed {
		m.requests++
	}
	m.mu.Unlock()
	if !allowed {
		modelFailure(w)
		return
	}
	// A non-rewindable body also prevents net/http from replaying a POST after
	// a transport failure. Wait for its actual transport close before clearing.
	input := &modelBody{reader: bytes.NewReader(body), closed: make(chan struct{})}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, m.endpoint, input)
	if err != nil {
		modelFailure(w)
		return
	}
	upstream.ContentLength = int64(len(body))
	upstream.Header.Set("Authorization", "Bearer "+string(m.key))
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	upstream.Header.Set("Accept-Encoding", "identity")
	response, err := m.client.Do(upstream)
	defer func() { <-input.closed }()
	if err != nil {
		modelFailure(w)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 ||
		response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		modelFailure(w)
		return
	}
	output, err := io.ReadAll(io.LimitReader(response.Body, modelResponseLimit+1))
	defer clear(output)
	if err != nil || len(output) > modelResponseLimit {
		modelFailure(w)
		return
	}
	clean, err := protectModelJSON(output, m.protectText)
	defer clear(clean)
	if err != nil || len(clean) > modelResponseLimit {
		modelFailure(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(clean)
}

// constrainResponseAllowance bounds the provider request's per-response
// allowance. It does not measure actual generated tokens, reasoning or cost.
func (m *modelTransport) constrainResponseAllowance(request map[string]any) bool {
	for _, name := range []string{"max_output_tokens", "max_new_tokens", "max_length", "min_tokens", "min_new_tokens", "min_length",
		"generation_config", "generationConfig", "extra_body", "best_of"} {
		if _, present := request[name]; present {
			return false
		}
	}
	if value, present := request["n"]; present {
		number, ok := value.(json.Number)
		count, err := number.Int64()
		if !ok || err != nil || count != 1 {
			return false
		}
	}
	legacy, hasLegacy := request["max_tokens"]
	completion, hasCompletion := request["max_completion_tokens"]
	if hasLegacy && hasCompletion {
		return false
	}
	name, allowance := "max_tokens", int64(m.maxTokens)
	if hasLegacy || hasCompletion {
		value := legacy
		if hasCompletion {
			name, value = "max_completion_tokens", completion
		}
		number, ok := value.(json.Number)
		requested, err := number.Int64()
		if !ok || err != nil || requested < 1 {
			return false
		}
		if requested < allowance {
			allowance = requested
		}
	}
	// New canonical metadata must obey the same known-value protection as the
	// original body, including a numeric value introduced by clamping.
	if m.protectText(name) != name || m.protectText(strconv.FormatInt(allowance, 10)) != strconv.FormatInt(allowance, 10) {
		return false
	}
	request[name] = allowance
	return true
}

// Canonicalization must not silently pick one competing top-level allowance.
func uniqueModelRequestFields(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		clear(value)
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}

type modelBody struct {
	reader  *bytes.Reader
	closed  chan struct{}
	mu      sync.Mutex
	stopped bool
	once    sync.Once
}

func (body *modelBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.stopped {
		return 0, io.EOF
	}
	return body.reader.Read(buffer)
}

func (body *modelBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.stopped = true
	body.once.Do(func() { close(body.closed) })
	return nil
}

func modelFailure(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest) // Non-retryable; no provider error/headers escape.
	_, _ = io.WriteString(w, `{"error":{"message":"Model request unavailable","type":"invalid_request_error"}}`)
}

func (m *modelTransport) writeJSON(w http.ResponseWriter, value any) {
	encoded, err := json.Marshal(value)
	defer clear(encoded)
	if err != nil || len(encoded) > modelResponseLimit {
		modelFailure(w)
		return
	}
	protected, err := protectModelJSON(encoded, m.protectText)
	defer clear(protected)
	if err != nil || len(protected) > modelResponseLimit {
		modelFailure(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(protected)
}

func (m *modelTransport) cleanJSON(raw []byte, maximum int) (value any, err error) {
	if len(raw) > maximum {
		return nil, errModelTransport
	}
	encoded, err := protectModelJSON(raw, m.protectText)
	defer clear(encoded)
	if err != nil {
		return nil, err
	}
	return decodeModelJSON(encoded)
}

func (m *modelTransport) protectText(value string) string {
	return strings.ReplaceAll(m.redact(value), string(m.token), "[REDACTED]")
}

// protectModelJSON is the common private observation/model boundary. JSON
// strings can themselves contain encoded tool arguments; decode those before
// scrubbing so Unicode escapes cannot conceal an otherwise exact known value.
func protectModelJSON(raw []byte, redact func(string) string) (encoded []byte, err error) {
	defer func() {
		if recover() != nil {
			encoded, err = nil, errModelTransport
		}
	}()
	if len(raw) > modelRequestLimit || !utf8.Valid(raw) || redact == nil {
		return nil, errModelTransport
	}
	value, err := decodeModelJSON(raw)
	if err != nil {
		return nil, errModelTransport
	}
	value, err = protectModelValue(value, 0, redact)
	if err != nil {
		return nil, err
	}
	encoded, err = json.Marshal(value)
	if err != nil || len(encoded) > modelRequestLimit {
		clear(encoded)
		return nil, errModelTransport
	}
	return encoded, nil
}

func decodeModelJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, errModelTransport
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errModelTransport
	}
	return value, nil
}

func protectModelValue(value any, depth int, redact func(string) string) (any, error) {
	if depth > modelJSONDepth {
		return nil, errModelTransport
	}
	switch value := value.(type) {
	case json.Number:
		if text := value.String(); redact(text) != text {
			return nil, errModelTransport
		}
	case string:
		clean := redact(value)
		trimmed := strings.TrimSpace(clean)
		if len(trimmed) > 1 && strings.ContainsRune("{[\"", rune(trimmed[0])) {
			if nested, err := decodeModelJSON([]byte(trimmed)); err == nil {
				nested, err = protectModelValue(nested, depth+1, redact)
				if err != nil {
					return nil, err
				}
				encoded, err := json.Marshal(nested)
				if err != nil {
					return nil, errModelTransport
				}
				clean = string(encoded)
			}
		}
		return clean, nil
	case map[string]any:
		clean := make(map[string]any, len(value))
		for key, child := range value {
			name, err := protectModelValue(key, depth+1, redact)
			if err != nil {
				return nil, err
			}
			text := name.(string)
			if _, exists := clean[text]; exists {
				return nil, errModelTransport
			}
			clean[text], err = protectModelValue(child, depth+1, redact)
			if err != nil {
				return nil, err
			}
		}
		return clean, nil
	case []any:
		for i := range value {
			var err error
			value[i], err = protectModelValue(value[i], depth+1, redact)
			if err != nil {
				return nil, err
			}
		}
	}
	return value, nil
}

func (m *modelTransport) Close() error {
	m.close.Do(func() {
		m.mu.Lock()
		m.closing = true
		m.mu.Unlock()
		m.cancel()
		deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if m.server.Shutdown(deadline) != nil {
			m.closeErr = errModelTransport
			_ = m.server.Close()
		}
		m.http.CloseIdleConnections()
		go func() {
			<-m.served
			m.handlers.Wait()
			m.http.CloseIdleConnections()
			clear(m.key)
			clear(m.token)
			close(m.joined)
		}()
		select {
		case <-m.joined:
			m.mu.Lock()
			if m.fault {
				m.closeErr = errModelTransport
			}
			m.mu.Unlock()
		case <-deadline.Done():
			m.closeErr = errModelTransport
		}
	})
	return m.closeErr
}
