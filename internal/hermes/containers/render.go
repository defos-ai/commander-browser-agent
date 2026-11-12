package containers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/textrender"
)

const rendererExecutable = "/usr/local/lib/commander-browser/render-text"

type rendererExecution struct {
	ID          string `json:"ID"`
	ContainerID string `json:"ContainerID"`
	Running     *bool  `json:"Running"`
	ExitCode    *int   `json:"ExitCode"`
	Process     struct {
		EntryPoint string   `json:"entrypoint"`
		Arguments  []string `json:"arguments"`
		User       string   `json:"user"`
		Privileged bool     `json:"privileged"`
		Tty        bool     `json:"tty"`
	} `json:"ProcessConfig"`
}

// Render executes only the immutable image's fixed, bounded renderer helper.
// It receives protected text over private stdin and returns PNG over a separate
// binary stream; neither CDP nor pixels cross the worker's JSON protocol.
func (instance *Instance) Render(ctx context.Context, endpoint, text string, width, height int) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, operationError(ctx, ErrUnavailable)
	}
	request, err := textrender.EncodeRelayRequest(textrender.RelayRequest{
		Version: 1, Endpoint: endpoint, Text: text, Width: width, Height: height,
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clear(request)
	active, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	state, err := instance.inspect(active)
	if err != nil || !*state.State.Running || state.State.Paused || state.State.Status != "running" {
		return nil, operationError(active, ErrUncertain)
	}
	create, _ := json.Marshal(struct {
		AttachStdin  bool     `json:"AttachStdin"`
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		Tty          bool     `json:"Tty"`
		Privileged   bool     `json:"Privileged"`
		User         string   `json:"User"`
		WorkingDir   string   `json:"WorkingDir"`
		Cmd          []string `json:"Cmd"`
	}{true, true, true, false, false, "1000:1000", "/home/commander", []string{rendererExecutable}})
	response, err := instance.request(active, http.MethodPost, apiPrefix+"/containers/"+state.ID+"/exec", create)
	var created struct {
		ID string `json:"Id"`
	}
	if err != nil || response.status != http.StatusCreated || json.Unmarshal(response.body, &created) != nil || !containerID(created.ID) {
		// Creating an exec does not start it. No helper was admitted without a
		// valid ID receipt, even when the create response itself was lost.
		return nil, operationError(active, ErrUnavailable)
	}
	if active.Err() != nil {
		return nil, operationError(active, ErrUnavailable) // No start has been attempted.
	}
	if execution, err := instance.inspectRenderer(active, created.ID); err != nil || *execution.Running {
		if active.Err() != nil {
			return nil, operationError(active, ErrUnavailable)
		}
		return nil, operationError(active, ErrUncertain)
	}
	if active.Err() != nil {
		return nil, operationError(active, ErrUnavailable)
	}
	stream, err := hijack(active, instance.config.Socket, apiPrefix+"/exec/"+created.ID+"/start",
		[]byte(`{"Detach":false,"Tty":false}`), true)
	if err != nil {
		// Start may have reached Docker. Closing a failed attachment makes its
		// stdin EOF cancel the helper, but only inspect can confirm its exit.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		exit, joined := instance.joinRenderer(cleanup, created.ID)
		if joined && exit == textrender.RelayExitCanceled && active.Err() != nil {
			return nil, operationError(active, ErrUnavailable)
		}
		return nil, operationError(active, ErrUncertain)
	}
	defer stream.close()
	input := &attachInput{stream: stream}
	// A caller cancellation interrupts even a partially written request. The
	// helper keeps stdin open as a liveness lease and joins before exit2.
	stopInput := context.AfterFunc(active, func() { _ = input.Close() })
	defer stopInput()
	deadline := time.Now().Add(5 * time.Second)
	if callerDeadline, ok := active.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	if input.SetWriteDeadline(deadline) != nil {
		_ = input.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = instance.joinRenderer(cleanup, created.ID)
		stop()
		return nil, ErrUncertain
	}
	written, writeErr := input.Write(request)
	if written != len(request) && writeErr == nil {
		writeErr = ErrStream
	}
	_ = input.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		_ = input.Close()
	}
	type imageResult struct {
		data []byte
		err  error
	}
	readDone := make(chan imageResult, 1)
	go func() {
		data, err := textrender.ReadRelayPNG(&stream.output, width, height)
		readDone <- imageResult{data, err}
	}()
	var image imageResult
	var cleanup context.Context
	var stopCleanup context.CancelFunc
	select {
	case image = <-readDone:
		cleanup, stopCleanup = context.WithTimeout(context.Background(), 3*time.Second)
	case <-active.Done():
		cleanup, stopCleanup = context.WithTimeout(context.Background(), 3*time.Second)
		_ = input.Close()
		select {
		case image = <-readDone:
		case <-cleanup.Done():
			_ = stream.close()
			image = <-readDone // Closing the owned Unix socket joins its reader.
		}
	}
	defer stopCleanup()
	defer func() { clear(image.data) }()
	if image.err != nil {
		// Invalid binary output is never forwarded or truncated. Closing both
		// directions also releases a helper blocked while writing that output.
		_ = stream.close()
	}
	exit, joined := instance.joinRenderer(cleanup, created.ID)
	if !joined {
		return nil, operationError(active, ErrUncertain)
	}
	if active.Err() != nil && (exit == textrender.RelayExitCanceled || exit == textrender.RelayExitSuccess) {
		return nil, operationError(active, ErrUnavailable)
	}
	if exit != textrender.RelayExitSuccess || writeErr != nil || image.err != nil {
		return nil, operationError(active, ErrUncertain)
	}
	result := image.data
	image.data = nil
	return result, nil
}

func (instance *Instance) inspectRenderer(ctx context.Context, identifier string) (rendererExecution, error) {
	response, err := instance.request(ctx, http.MethodGet, apiPrefix+"/exec/"+identifier+"/json", nil)
	var execution rendererExecution
	if err != nil || response.status != http.StatusOK || json.Unmarshal(response.body, &execution) != nil ||
		execution.ID != identifier || execution.ContainerID != instance.ID() || execution.Running == nil ||
		execution.Process.EntryPoint != rendererExecutable || len(execution.Process.Arguments) != 0 ||
		execution.Process.User != "1000:1000" || execution.Process.Privileged || execution.Process.Tty {
		return rendererExecution{}, operationError(ctx, ErrUncertain)
	}
	return execution, nil
}

func (instance *Instance) joinRenderer(ctx context.Context, identifier string) (int, bool) {
	for ctx.Err() == nil {
		execution, err := instance.inspectRenderer(ctx, identifier)
		if err != nil {
			return 0, false
		}
		if !*execution.Running {
			// A created exec has no exit code until its helper actually exits.
			// Not-running alone is not a stopped-process receipt.
			if execution.ExitCode == nil {
				return 0, false
			}
			return *execution.ExitCode, true
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return 0, false
		}
	}
	return 0, false
}
