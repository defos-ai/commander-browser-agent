// Package containers owns isolated, pinned Hermes worker containers through a
// local Docker Unix socket. It does not expose an agent action or pull images.
package containers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/defos-ai/commander/framework/id"
)

const apiVersion = "1.47"
const apiPrefix = "/v" + apiVersion
const maxResponse = 1 << 20

const labelType = "io.defos.commander.browser.worker"
const labelSession = "io.defos.commander.browser.session"
const labelOwner = "io.defos.commander.browser.owner"

var ErrUnavailable = errors.New("isolated Hermes container is unavailable")
var ErrUncertain = errors.New("isolated Hermes container ownership is uncertain")
var ErrStream = errors.New("isolated Hermes container stream is invalid")

// Config contains trusted host settings, never business input or credentials.
// Image is a local immutable image ID, not a tag or a registry reference.
type Config struct {
	Socket  string
	Image   string
	Session string
}

// Instance retains ownership even when create/start receipts are uncertain.
// There is no automatic removal; the owner must stop, confirm and remove it.
type Instance struct {
	config Config
	client *http.Client

	mu               sync.Mutex
	ref              string
	nonce            string
	volume           string
	volumeCreated    string
	stream           *attachment
	stopped          bool
	containerRemoved bool
	removed          bool
}

// ID returns an opaque Docker resource reference. Before a verified ID receipt
// it is the reserved random name, so an uncertain create remains inspectable.
func (instance *Instance) ID() string {
	instance.mu.Lock()
	defer instance.mu.Unlock()
	return instance.ref
}

func (instance *Instance) Input() io.WriteCloser {
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if instance.stream == nil {
		return unavailableInput{}
	}
	return &attachInput{stream: instance.stream}
}

func (instance *Instance) Output() io.ReadCloser {
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if instance.stream == nil {
		return unavailableOutput{}
	}
	return &instance.stream.output
}

// Launch attaches before starting the pinned command. A successful stream has
// an independent lifetime after startup; cancelling ctx later cannot detach it.
// Any error after a create attempt returns its Instance for explicit cleanup.
func Launch(ctx context.Context, config Config) (*Instance, error) {
	if !filepath.IsAbs(config.Socket) || len(config.Socket) > 4096 || strings.ContainsRune(config.Socket, 0) ||
		!imageID(config.Image) || !id.Valid(config.Session) {
		return nil, ErrUnavailable
	}
	startup, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	client := newClient(config.Socket)
	instance := &Instance{config: config, client: client}
	if err := instance.readiness(startup); err != nil {
		return nil, operationError(startup, ErrUnavailable)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, ErrUnavailable
	}
	instance.nonce = hex.EncodeToString(random[:])
	instance.ref = "commander-hermes-worker-" + instance.nonce
	clear(random[:])
	payload, err := json.Marshal(instance.configuration())
	if err != nil {
		return nil, ErrUnavailable
	}
	response, err := instance.request(startup, http.MethodPost, apiPrefix+"/containers/create?name="+url.QueryEscape(instance.ref), payload)
	if err != nil {
		return instance, operationError(startup, ErrUncertain)
	}
	var created struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if response.status != http.StatusCreated || json.Unmarshal(response.body, &created) != nil || !containerID(created.ID) {
		return instance, operationError(startup, ErrUncertain)
	}
	instance.ref = created.ID
	if len(created.Warnings) != 0 {
		// A warning can mean resource controllers ignored requested limits.
		// Keep ownership, but never start a worker with an unconfirmed policy.
		return instance, operationError(startup, ErrUncertain)
	}
	if _, err := instance.inspect(startup); err != nil {
		return instance, operationError(startup, ErrUncertain)
	}
	stream, err := attach(startup, config.Socket, instance.ref)
	if err != nil {
		return instance, operationError(startup, ErrUncertain)
	}
	instance.stream = stream
	response, err = instance.request(startup, http.MethodPost, apiPrefix+"/containers/"+instance.ref+"/start", nil)
	if err != nil || response.status != http.StatusNoContent || startup.Err() != nil {
		_ = instance.CloseIO()
		return instance, operationError(startup, ErrUncertain)
	}
	return instance, nil
}

func newClient(socket string) *http.Client {
	// Wait may deliberately keep its response headers pending for the entire
	// caller allowance. All requests are bounded by their context instead.
	transport := &http.Transport{DisableKeepAlives: true, DisableCompression: true,
		MaxResponseHeaderBytes: maxAttachHeader,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnavailable }}
}

type apiResponse struct {
	status int
	body   []byte
}

func (instance *Instance) request(ctx context.Context, method, path string, body []byte) (apiResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, bytes.NewReader(body))
	if err != nil {
		return apiResponse{}, ErrUnavailable
	}
	request.GetBody = nil // Never replay a mutation after an uncertain delivery.
	request.Header.Set("Content-Type", "application/json")
	response, err := instance.client.Do(request)
	if err != nil {
		return apiResponse{}, operationError(ctx, ErrUnavailable)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return apiResponse{}, operationError(ctx, ErrUnavailable)
	}
	return apiResponse{status: response.StatusCode, body: data}, nil
}

func (instance *Instance) readiness(ctx context.Context) error {
	response, err := instance.request(ctx, http.MethodGet, "/version", nil)
	var version struct {
		API string `json:"ApiVersion"`
		Min string `json:"MinAPIVersion"`
	}
	if err != nil || response.status != http.StatusOK || json.Unmarshal(response.body, &version) != nil ||
		!versionAtLeast(version.API, apiVersion) || !versionAtLeast(apiVersion, version.Min) {
		return ErrUnavailable
	}
	response, err = instance.request(ctx, http.MethodGet, apiPrefix+"/images/"+instance.config.Image+"/json", nil)
	var image struct {
		ID     string                                       `json:"Id"`
		Config struct{ Volumes map[string]json.RawMessage } `json:"Config"`
	}
	// Image-declared volumes would create anonymous mounts even with no binds.
	if err != nil || response.status != http.StatusOK || json.Unmarshal(response.body, &image) != nil ||
		image.ID != instance.config.Image || len(image.Config.Volumes) != 0 {
		return ErrUnavailable
	}
	return nil
}

func versionAtLeast(version, minimum string) bool {
	parse := func(raw string) (int, bool) {
		parts := strings.Split(raw, ".")
		if len(parts) != 2 || parts[0] != "1" || len(parts[1]) > 4 || parts[1] == "" {
			return 0, false
		}
		minor, err := strconv.Atoi(parts[1])
		for _, character := range parts[1] {
			if character < '0' || character > '9' {
				return 0, false
			}
		}
		return minor, err == nil && minor >= 0
	}
	left, validLeft := parse(version)
	right, validRight := parse(minimum)
	return validLeft && validRight && left >= right
}

func imageID(value string) bool {
	return strings.HasPrefix(value, "sha256:") && containerID(strings.TrimPrefix(value, "sha256:"))
}

func containerID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func operationError(ctx context.Context, fixed error) error {
	if ctx.Err() != nil {
		return errors.Join(fixed, ctx.Err(), context.Cause(ctx))
	}
	return fixed
}
