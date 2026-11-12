package textrender

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/defos-ai/commander/framework/agent/action"
)

const RelayRequestLimit = 400 << 10
const RelayPNGLimit = pngBytes

const (
	RelayExitSuccess  = 0
	RelayExitFailure  = 1
	RelayExitCanceled = 2
)

var relayMagic = [8]byte{'C', 'B', 'R', 'P', 'N', 'G', '0', '1'}
var errRelay = errors.New("protected renderer protocol is invalid")

// RelayRequest is private helper input. Text must already be protected by the
// trusted host; the helper receives neither source DOM nor browser credentials.
type RelayRequest struct {
	Version  int    `json:"version"`
	Endpoint string `json:"endpoint"`
	Text     string `json:"text"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

func (request RelayRequest) Validate() error {
	endpoint, err := url.Parse(request.Endpoint)
	if err != nil || request.Version != 1 || len(request.Endpoint) > 2048 || endpoint.Scheme != "ws" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" ||
		strings.Contains(request.Endpoint, "#") || endpoint.Opaque != "" || endpoint.RawPath != "" ||
		endpoint.Path != "" && endpoint.Path != "/" {
		return errRelay
	}
	ip := net.ParseIP(endpoint.Hostname())
	port, err := strconv.Atoi(endpoint.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return errRelay
	}
	return Validate(request.Text, request.Width, request.Height)
}

// EncodeRelayRequest uses a length-prefixed canonical request. The pipe remains
// open afterward: EOF is cancellation, never the normal end of this request.
func EncodeRelayRequest(request RelayRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > RelayRequestLimit {
		clear(body)
		return nil, errRelay
	}
	defer clear(body)
	encoded := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(encoded, uint32(len(body)))
	copy(encoded[4:], body)
	return encoded, nil
}

func readRelayRequest(input io.Reader) (RelayRequest, error) {
	var header [4]byte
	if _, err := io.ReadFull(input, header[:]); err != nil {
		return RelayRequest{}, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > RelayRequestLimit {
		return RelayRequest{}, errRelay
	}
	body := make([]byte, int(length))
	defer clear(body)
	if _, err := io.ReadFull(input, body); err != nil {
		return RelayRequest{}, err
	}
	var request RelayRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Validate() != nil {
		return RelayRequest{}, errRelay
	}
	// Only the canonical encoding produced by the host is accepted. This also
	// refuses duplicate fields, alternate escaping and trailing JSON values.
	canonical, err := json.Marshal(request)
	defer clear(canonical)
	if err != nil || !bytes.Equal(canonical, body) {
		return RelayRequest{}, errRelay
	}
	return request, nil
}

// ReadRelayPNG accepts exactly one bounded binary image and then requires EOF.
// The caller separately verifies the helper's zero-exit receipt.
func ReadRelayPNG(input io.Reader, width, height int) ([]byte, error) {
	var header [12]byte
	if _, err := io.ReadFull(input, header[:]); err != nil || !bytes.Equal(header[:8], relayMagic[:]) {
		return nil, errRelay
	}
	length := binary.BigEndian.Uint32(header[8:])
	if length == 0 || length > RelayPNGLimit {
		return nil, errRelay
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(input, data); err != nil {
		clear(data)
		return nil, errRelay
	}
	var extra [1]byte
	if count, err := input.Read(extra[:]); count != 0 || err != io.EOF {
		clear(data)
		return nil, errRelay
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || configuration.Width != width || configuration.Height != height {
		clear(data)
		return nil, errRelay
	}
	return data, nil
}

func writeRelayPNG(output io.Writer, data []byte) error {
	if len(data) == 0 || len(data) > RelayPNGLimit {
		return errRelay
	}
	var header [12]byte
	copy(header[:8], relayMagic[:])
	binary.BigEndian.PutUint32(header[8:], uint32(len(data)))
	if count, err := output.Write(header[:]); err != nil || count != len(header) {
		return errRelay
	}
	if count, err := output.Write(data); err != nil || count != len(data) {
		return errRelay
	}
	return nil
}

// ServeRelay is the fixed in-image helper. It emits no diagnostics and writes
// image bytes only after every owned renderer resource has joined. The input
// pipe is a liveness lease throughout rendering; EOF or extra bytes cancel it.
func ServeRelay(parent context.Context, input io.ReadCloser, output io.Writer) int {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stopClose()
	request, err := readRelayRequest(input)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return RelayExitCanceled
		}
		return RelayExitFailure
	}
	finished, inputDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(inputDone)
		var extra [1]byte
		_, _ = input.Read(extra[:])
		select {
		case <-finished:
		default:
			cancel()
		}
	}()
	defer func() {
		close(finished)
		_ = input.Close()
		<-inputDone
	}()
	// The trusted host publishes the fixed progress message before dispatch.
	// Shared rendering must not try to publish another log from this private
	// helper, which has no Commander transport or execution providers.
	renderContext := action.WithLogs(ctx, relayLogs{})
	owner := New(renderContext, nil)
	data, renderErr := owner.Render(renderContext, renderContext, request.Endpoint, request.Text, request.Width, request.Height)
	defer clear(data)
	cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
	cleanErr := owner.Wait(cleanup)
	stop()
	if cleanErr != nil {
		return RelayExitFailure
	}
	if ctx.Err() != nil {
		return RelayExitCanceled
	}
	if renderErr != nil || writeRelayPNG(output, data) != nil {
		return RelayExitFailure
	}
	return RelayExitSuccess
}

type relayLogs struct{}

func (relayLogs) Log(ctx context.Context, _ string) error { return ctx.Err() }
