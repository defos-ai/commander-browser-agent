package hermes

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/hermes/containers"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// ContainerConfig selects a trusted, preloaded image and its exact creation
// claim. These are host settings, never model settings or action inputs.
type ContainerConfig struct {
	Socket string
	Image  string
	Claim  sessions.CreationClaim
}

// StartContainer stages the isolated worker transport independently of the
// session manager and public action catalog. A partial launch retains a worker
// handle and private container state when cleanup cannot be confirmed.
func StartContainer(ctx context.Context, config ContainerConfig) (*Worker, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if config.Claim.Validate() != nil {
		return nil, ErrUnavailable
	}
	instance, err := containers.Launch(ctx, containers.Config{Socket: config.Socket, Image: config.Image, Session: config.Claim.ID})
	if instance == nil {
		return nil, errors.Join(ErrUnavailable, contextError(ctx))
	}
	w := newWorker(Config{Python: "/opt/hermes/.venv/bin/python", Source: "/opt/hermes", Lightpanda: "/usr/local/bin/lightpanda", Claim: config.Claim})
	w.container = instance
	w.waitCtx, w.stopWait = context.WithCancel(context.Background())
	input, compatible := instance.Input().(commandInput)
	if err != nil || !compatible {
		// No healthy transport exists. Keep the owned reference even after a
		// best-effort stop; an absent protocol receipt never permits deletion.
		w.input, w.output = missingContainerInput{}, io.NopCloser(strings.NewReader(""))
		w.fault, w.waitErr = true, ErrUncertain
		_ = instance.CloseIO()
		close(w.readDone)
		close(w.procDone)
		_ = w.Close()
		return w, errors.Join(ErrUnavailable, ErrUncertain, contextError(ctx))
	}
	w.input, w.output = input, instance.Output()
	go w.readFrames()
	go w.waitProcess()
	startup, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	select {
	case err := <-w.readyDone:
		if err == nil && startup.Err() == nil && w.lifetime.Err() == nil {
			return w, nil
		}
	case <-startup.Done():
	case <-w.procDone:
	}
	closeErr := w.Close()
	if closeErr != nil {
		return w, errors.Join(ErrUnavailable, contextError(startup), closeErr)
	}
	return nil, errors.Join(ErrUnavailable, contextError(startup))
}

type missingContainerInput struct{}

func (missingContainerInput) Write([]byte) (int, error)        { return 0, ErrUnavailable }
func (missingContainerInput) Close() error                     { return nil }
func (missingContainerInput) SetWriteDeadline(time.Time) error { return ErrUnavailable }

// Relay messages bypass action event handlers entirely. There is one bounded
// provider exchange in flight; reservations also join response delivery.
func (w *Worker) relayModel(message frame, model *modelTransport) bool {
	defer clear(message.Payload)
	if model.ctx.Err() != nil {
		return true // Cancellation wakes the original Python waiter, without forwarding.
	}
	var request modelRelayRequest
	if strictJSON(message.Payload, &request) != nil || len(request.Body) > modelRelayLimit ||
		!boundedText(request.Method, 8, true) || !boundedText(request.Path, 64, true) || !boundedText(request.Token, 128, true) {
		clear(request.Body)
		w.fail(ErrProtocol)
		return false
	}
	if !model.beginRelay(message.ID) {
		clear(request.Body)
		if model.ctx.Err() != nil {
			return true
		}
		w.fail(ErrProtocol)
		return false
	}
	identifier := message.ID
	go func() {
		defer model.endRelay(identifier)
		defer clear(request.Body)
		reply := model.relay(model.ctx, request)
		model.finishRelayWork(identifier)
		defer clear(reply.Body)
		if model.ctx.Err() != nil || w.lifetime.Err() != nil {
			return
		}
		if err := w.send(model.ctx, "model_reply", identifier, reply); err != nil && model.ctx.Err() == nil {
			w.fail(ErrProtocol)
		}
	}()
	return true
}
