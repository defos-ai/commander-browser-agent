package containers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxAttachHeader = 16 << 10
const maxAttachFrame = 1 << 20

type attachment struct {
	connection net.Conn
	output     attachOutput
	closeOnce  sync.Once
	writeMu    sync.Mutex
}

func attach(ctx context.Context, socket, reference string) (*attachment, error) {
	return hijack(ctx, socket, apiPrefix+"/containers/"+reference+"/attach?stream=1&stdin=1&stdout=1&stderr=1", nil, false)
}

// hijack shares Docker's bounded multiplexed framing with the fixed renderer
// exec. Renderer stderr is a protocol failure rather than diagnostic output.
func hijack(ctx context.Context, socket, path string, body []byte, strictOutput bool) (*attachment, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, operationError(ctx, ErrUnavailable)
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); close(joined) })
	watchStopped := false
	stopWatch := func() bool {
		stopped := stop()
		if !stopped {
			<-joined
		}
		watchStopped = true
		return stopped
	}
	settled := false
	defer func() {
		if !settled {
			if !watchStopped {
				stopWatch()
			}
			_ = connection.Close()
		}
	}()
	request, err := http.NewRequest(http.MethodPost, "http://docker"+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "tcp")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if request.Write(connection) != nil {
		return nil, operationError(ctx, ErrUnavailable)
	}
	reader := bufio.NewReader(connection)
	// Parse only a bounded header copy, retaining bytes already buffered after
	// it. HTTP 200/101 hijacks carry Docker frames, not an ordinary HTTP body.
	var header bytes.Buffer
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil || header.Len()+len(line) > maxAttachHeader {
			return nil, operationError(ctx, ErrStream)
		}
		header.Write(line)
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header.Bytes())), request)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusOK {
		return nil, operationError(ctx, ErrUnavailable)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/vnd.docker.raw-stream" && media != "application/vnd.docker.multiplexed-stream" ||
		len(response.TransferEncoding) != 0 || response.StatusCode == http.StatusSwitchingProtocols &&
		(!headerToken(response.Header.Get("Connection"), "upgrade") || !strings.EqualFold(response.Header.Get("Upgrade"), "tcp")) {
		return nil, ErrStream
	}
	if !stopWatch() {
		return nil, operationError(ctx, ErrUnavailable)
	}
	if ctx.Err() != nil {
		return nil, operationError(ctx, ErrUnavailable)
	}
	stream := &attachment{connection: connection}
	stream.output = attachOutput{stream: stream, reader: reader, strict: strictOutput}
	settled = true
	return stream, nil
}

func headerToken(value, expected string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), expected) {
			return true
		}
	}
	return false
}

func (stream *attachment) close() error {
	stream.closeOnce.Do(func() { _ = stream.connection.Close() })
	return nil
}

type attachInput struct{ stream *attachment }

// SetWriteDeadline lets the owning bridge bound writes to a stalled worker.
func (input *attachInput) SetWriteDeadline(deadline time.Time) error {
	if input.stream.connection.SetWriteDeadline(deadline) != nil {
		return ErrStream
	}
	return nil
}

func (input *attachInput) Write(data []byte) (int, error) {
	input.stream.writeMu.Lock()
	defer input.stream.writeMu.Unlock()
	written, err := input.stream.connection.Write(data)
	if err != nil {
		return written, ErrStream
	}
	return written, nil
}

func (input *attachInput) Close() error {
	if socket, ok := input.stream.connection.(*net.UnixConn); ok {
		if err := socket.CloseWrite(); err != nil {
			return ErrStream
		}
		return nil
	}
	return input.stream.close()
}

type attachOutput struct {
	stream    *attachment
	reader    *bufio.Reader
	mu        sync.Mutex
	remaining uint32
	failed    bool
	strict    bool
}

func (output *attachOutput) Read(buffer []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(buffer) == 0 {
		return 0, nil
	}
	if output.failed {
		return 0, ErrStream
	}
	for output.remaining == 0 {
		var header [8]byte
		count, err := io.ReadFull(output.reader, header[:])
		if err == io.EOF && count == 0 {
			return 0, io.EOF
		}
		length := binary.BigEndian.Uint32(header[4:])
		if err != nil || header[1] != 0 || header[2] != 0 || header[3] != 0 || header[0] > 2 || length > maxAttachFrame {
			return output.fail()
		}
		if header[0] == 2 {
			if output.strict && length != 0 {
				return output.fail()
			}
			if _, err := io.CopyN(io.Discard, output.reader, int64(length)); err != nil {
				return output.fail()
			}
			continue
		}
		output.remaining = length // Docker stream 0 aliases stdout; 1 is stdout.
	}
	wanted := min(len(buffer), int(output.remaining))
	count, err := output.reader.Read(buffer[:wanted])
	output.remaining -= uint32(count)
	if err != nil {
		_, streamErr := output.fail()
		return count, streamErr
	}
	return count, nil
}

func (output *attachOutput) fail() (int, error) {
	output.failed = true
	_ = output.stream.close()
	return 0, ErrStream
}

func (output *attachOutput) Close() error { return output.stream.close() }

type unavailableInput struct{}

func (unavailableInput) Write([]byte) (int, error) { return 0, ErrUnavailable }
func (unavailableInput) Close() error              { return nil }

type unavailableOutput struct{}

func (unavailableOutput) Read([]byte) (int, error) { return 0, ErrUnavailable }
func (unavailableOutput) Close() error             { return nil }
