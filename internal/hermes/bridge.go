package hermes

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrUnavailable  = errors.New("Hermes worker is unavailable")
	ErrProtocol     = errors.New("Hermes worker protocol failed")
	ErrUncertain    = errors.New("Hermes worker cleanup or browser state is uncertain")
	ErrClosed       = errors.New("Hermes worker is closed")
	ErrNotConnected = errors.New("Hermes browser has not connected")
	ErrUnverified   = errors.New("Hermes browser connection could not be verified")
	ErrLost         = errors.New("Hermes browser identity is lost")
	errHandler      = errors.New("Hermes host event was not accepted")
	errRequest      = errors.New("invalid Hermes execution request")
)

const maxFrame = 1 << 20

type frame struct {
	Version int             `json:"version"`
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

type health struct {
	State string `json:"state"`
}

// Worker owns one private host bridge and its original creation claim. A lost
// worker cannot be restarted or rebound; the manager must allocate a new one.
type Worker struct {
	config Config
	work   string
	cmd    *exec.Cmd
	input  *os.File
	output *os.File

	lifetime  context.Context
	cancel    context.CancelCauseFunc
	writeGate chan struct{}
	gate      chan struct{}
	mu        sync.Mutex
	ready     bool
	closing   bool
	fault     bool
	runID     string
	run       chan frame
	closeID   string
	ack       bool
	clean     bool
	waitErr   error
	closeErr  error
	health    map[string]chan health

	readyDone chan error
	procDone  chan struct{}
	readDone  chan struct{}
	closedAck chan struct{}
	closeDone chan struct{}
	closeOnce sync.Once
}

// Start checks readiness only. It never supplies model credentials or starts a
// browser. The caller's cancellation stops startup, not a successfully started
// worker's subsequent lifetime. A failed startup returns a non-nil worker when
// cleanup is uncertain, so its owner can retain the lost resource reference.
func Start(ctx context.Context, config Config) (*Worker, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := requireProcessOwnership(); err != nil {
		return nil, err
	}
	if config.Claim.Validate() != nil || !validPath(config.Python, false) ||
		!validPath(config.Source, true) || !validPath(config.Lightpanda, false) {
		return nil, ErrUnavailable
	}
	work, err := os.MkdirTemp("", "commander-hermes-")
	if err != nil {
		return nil, ErrUnavailable
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(work)
		}
	}()
	if err := stageWorker(work); err != nil {
		return nil, ErrUnavailable
	}
	home, temporary := filepath.Join(work, "home"), filepath.Join(work, "tmp")
	for _, directory := range []string{home, temporary} {
		if err := os.Mkdir(directory, 0700); err != nil {
			return nil, ErrUnavailable
		}
	}
	commands, input, err := os.Pipe()
	if err != nil {
		return nil, ErrUnavailable
	}
	output, events, err := os.Pipe()
	if err != nil {
		_ = commands.Close()
		_ = input.Close()
		return nil, ErrUnavailable
	}
	command := exec.Command(config.Python, "-I", filepath.Join(work, "worker.py"),
		"--source", config.Source, "--session", config.Claim.ID,
		"--lightpanda", config.Lightpanda, "--home", home)
	command.Dir = work
	command.Env = []string{
		"HOME=" + home, "HERMES_HOME=" + home, "HERMES_INSTALL_ROOT=" + config.Source,
		"TMPDIR=" + temporary, "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
		"PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1", "HERMES_DISABLE_LAZY_INSTALLS=1",
		"ANONYMIZED_TELEMETRY=false", "LIGHTPANDA_DISABLE_TELEMETRY=true", "LIGHTPANDA_DISABLE_CORE_DUMP=true",
		"LIGHTPANDA_PATH=" + config.Lightpanda, "BH_REQUIRE_EXISTING_DAEMON=1",
	}
	command.ExtraFiles = []*os.File{commands, events} // private command FD3 / event FD4
	command.Stdout, command.Stderr = io.Discard, io.Discard
	configureProcess(command)
	if err := command.Start(); err != nil {
		_ = commands.Close()
		_ = input.Close()
		_ = output.Close()
		_ = events.Close()
		return nil, ErrUnavailable
	}
	started = true
	_ = commands.Close()
	_ = events.Close()
	lifetime, cancel := context.WithCancelCause(context.Background())
	w := &Worker{config: config, work: work, cmd: command, input: input, output: output,
		lifetime: lifetime, cancel: cancel, gate: make(chan struct{}, 1), readyDone: make(chan error, 1),
		writeGate: make(chan struct{}, 1), health: make(map[string]chan health),
		procDone: make(chan struct{}), readDone: make(chan struct{}), closedAck: make(chan struct{}), closeDone: make(chan struct{})}
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
	err = errors.Join(ErrUnavailable, contextError(startup), closeErr)
	if errors.Is(closeErr, ErrUncertain) {
		return w, err
	}
	return nil, err
}

func validPath(path string, directory bool) bool {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && (directory && info.IsDir() || !directory && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0)
}

func stageWorker(root string) error {
	return fs.WalkDir(workerFiles, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		destination := filepath.Join(root, filepath.FromSlash(path))
		if entry.IsDir() {
			return os.Mkdir(destination, 0700)
		}
		data, err := workerFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0600)
	})
}

func (w *Worker) waitProcess() {
	err := w.cmd.Wait()
	w.mu.Lock()
	w.waitErr = err
	unexpected := !w.closing || err != nil
	if unexpected {
		w.fault = true
	}
	w.mu.Unlock()
	close(w.procDone)
	if unexpected {
		w.fail(ErrUncertain)
	}
}

func (w *Worker) fail(cause error) {
	w.mu.Lock()
	w.fault = true
	w.mu.Unlock()
	w.cancel(errors.Join(ErrLost, cause))
	select {
	case w.readyDone <- cause:
	default:
	}
	go w.Close()
}

func (w *Worker) readFrames() {
	defer close(w.readDone)
	scanner := bufio.NewScanner(w.output)
	scanner.Buffer(make([]byte, 4096), maxFrame+1)
	for scanner.Scan() {
		data := scanner.Bytes()
		var message frame
		if len(data) > maxFrame || strictJSON(data, &message) != nil || message.Version != 1 ||
			len(message.ID) > 128 || !utf8.ValidString(message.ID) || len(message.Payload) == 0 {
			w.fail(ErrProtocol)
			return
		}
		if !w.route(message) {
			return
		}
	}
	w.mu.Lock()
	expected := w.closing && w.ack
	w.mu.Unlock()
	if !expected {
		w.fail(ErrProtocol)
	}
}

func (w *Worker) route(message frame) bool {
	w.mu.Lock()
	if !w.ready && !w.closing && message.Kind == "ready" && message.ID == "" {
		var pins struct {
			Hermes string `json:"hermes"`
		}
		if strictJSON(message.Payload, &pins) == nil && pins.Hermes == "v0.23.1" {
			w.ready = true
			w.mu.Unlock()
			select {
			case w.readyDone <- nil:
			default:
			}
			return true
		}
	}
	if w.closing && message.Kind == "closed" && message.ID == w.closeID && !w.ack {
		var closed struct {
			Clean bool `json:"clean"`
		}
		if strictJSON(message.Payload, &closed) == nil {
			w.ack, w.clean = true, closed.Clean
			w.mu.Unlock()
			close(w.closedAck)
			return true
		}
	}
	if message.Kind == "health" {
		target, expected := w.health[message.ID]
		var status health
		if expected && strictJSON(message.Payload, &status) == nil &&
			(status.State == "starting" || status.State == "connected" || status.State == "unverified" || status.State == "lost") {
			delete(w.health, message.ID)
			w.mu.Unlock()
			if target != nil {
				target <- status
			}
			if status.State == "lost" {
				w.fail(ErrLost)
			}
			return true
		}
		w.mu.Unlock()
		w.fail(ErrProtocol)
		return false
	}
	current, target := w.runID, w.run
	valid := w.ready && target != nil && ((message.Kind == "event" && strings.HasPrefix(message.ID, current+":")) ||
		(message.Kind == "result" && message.ID == current))
	if valid && message.Kind == "result" {
		w.runID, w.run = "", nil
	}
	w.mu.Unlock()
	if !valid {
		w.fail(ErrProtocol)
		return false
	}
	select {
	case target <- message:
		return true
	case <-w.lifetime.Done():
		return true
	}
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrProtocol
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrProtocol
	}
	return nil
}

func freshID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(random[:]), nil
}

func (w *Worker) send(ctx context.Context, kind, id string, payload any) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ErrProtocol
	}
	defer clear(encoded)
	data, err := json.Marshal(frame{Version: 1, Kind: kind, ID: id, Payload: encoded})
	if err != nil || len(data) > maxFrame {
		clear(data)
		return ErrProtocol
	}
	defer clear(data)
	data = append(data, '\n')
	defer clear(data)
	select {
	case w.writeGate <- struct{}{}:
		defer func() { <-w.writeGate }()
	case <-ctx.Done():
		return contextError(ctx)
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if w.input.SetWriteDeadline(deadline) != nil {
		return ErrProtocol
	}
	n, err := w.input.Write(data)
	if err != nil || n != len(data) {
		if n == 0 && ctx.Err() != nil {
			return contextError(ctx)
		}
		return ErrProtocol
	}
	return nil
}

// Done closes when the owning worker closes or loses its original identity.
func (w *Worker) Done() <-chan struct{} { return w.lifetime.Done() }

// Verify observes the existing transport only. It neither enters the serialized
// execution gate nor creates a browser, reconnects, or selects another target.
func (w *Worker) Verify(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	identifier, err := freshID()
	if err != nil {
		return ErrUnverified
	}
	response := make(chan health, 1)
	w.mu.Lock()
	if w.fault {
		w.mu.Unlock()
		return ErrLost
	}
	if w.closing {
		w.mu.Unlock()
		return ErrClosed
	}
	if !w.ready || len(w.health) >= 16 {
		w.mu.Unlock()
		return ErrUnverified
	}
	w.health[identifier] = response
	w.mu.Unlock()
	// Keep abandoned IDs until their reply arrives, so a late receipt cannot be
	// mistaken for another check. The small pending limit bounds this state.
	defer func() {
		w.mu.Lock()
		if _, pending := w.health[identifier]; pending {
			w.health[identifier] = nil
		}
		w.mu.Unlock()
	}()
	check, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if err := w.send(check, "verify", identifier, struct{}{}); err != nil {
		if errors.Is(err, ErrProtocol) {
			w.fail(ErrProtocol)
			return errors.Join(ErrLost, ErrProtocol)
		}
		w.mu.Lock()
		delete(w.health, identifier) // no frame was written
		w.mu.Unlock()
		return errors.Join(ErrUnverified, contextError(check))
	}
	select {
	case status := <-response:
		w.mu.Lock()
		lost, closing := w.fault, w.closing
		w.mu.Unlock()
		if status.State == "lost" || lost {
			return ErrLost
		}
		if closing {
			return ErrClosed
		}
		if err := contextError(check); err != nil {
			return errors.Join(ErrUnverified, err)
		}
		switch status.State {
		case "connected":
			return nil
		case "starting":
			return ErrNotConnected
		default:
			return ErrUnverified
		}
	case <-check.Done():
		return errors.Join(ErrUnverified, contextError(check))
	case <-w.lifetime.Done():
		w.mu.Lock()
		lost := w.fault
		w.mu.Unlock()
		if lost {
			return ErrLost
		}
		return ErrClosed
	}
}

// Run serializes executions without holding the lifecycle lock. Controls may
// close the worker while a model call or host question is pending.
func (w *Worker) Run(ctx context.Context, request Request, handler Handler) (Result, error) {
	if !validRequest(request) {
		return Result{}, errRequest
	}
	select {
	case w.gate <- struct{}{}:
		defer func() { <-w.gate }()
	case <-ctx.Done():
		return Result{}, contextError(ctx)
	case <-w.lifetime.Done():
		return Result{}, errors.Join(ErrClosed, contextError(w.lifetime))
	}
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	identifier, err := freshID()
	if err != nil {
		return Result{}, err
	}
	frames := make(chan frame, 8)
	w.mu.Lock()
	if w.closing || w.fault || !w.ready {
		lost := w.fault
		w.mu.Unlock()
		if lost {
			return Result{}, errors.Join(ErrClosed, ErrLost)
		}
		return Result{}, ErrClosed
	}
	w.runID, w.run = identifier, frames
	w.mu.Unlock()
	run, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutSeconds)*time.Second)
	defer cancel()
	active, stop := context.WithCancelCause(run)
	defer stop(context.Canceled)
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-w.lifetime.Done():
			stop(context.Cause(w.lifetime))
		case <-watchDone:
		}
	}()
	if err := w.send(active, "run", identifier, request); err != nil {
		if !errors.Is(err, ErrProtocol) {
			w.mu.Lock()
			if w.runID == identifier {
				w.runID, w.run = "", nil
			}
			w.mu.Unlock()
			return Result{}, err // canceled before any command bytes were written
		}
		w.fail(ErrProtocol)
		return Result{}, errors.Join(err, contextError(active), w.Close())
	}
	seen := map[string]bool{}
	sequence, questions, progress := 0, 0, 0
	for {
		select {
		case <-active.Done():
			return Result{}, w.cancelRun(contextError(active), identifier, frames, nil)
		case message := <-frames:
			if active.Err() != nil {
				return Result{}, w.cancelRun(contextError(active), identifier, frames, &message)
			}
			if message.Kind == "result" {
				var result Result
				if strictJSON(message.Payload, &result) != nil || !validResult(result) {
					w.fail(ErrProtocol)
					return Result{}, errors.Join(ErrProtocol, w.Close())
				}
				result.Text = redactKey(result.Text, request.APIKey)
				clear(message.Payload)
				if result.Status == "uncertain" {
					w.fail(ErrUncertain)
					return result, errors.Join(ErrLost, ErrUncertain, w.Close())
				}
				return result, nil
			}
			var event Event
			if seen[message.ID] || strictJSON(message.Payload, &event) != nil || !validEvent(event) {
				w.fail(ErrProtocol)
				return Result{}, errors.Join(ErrProtocol, w.Close())
			}
			event.Text = redactKey(event.Text, request.APIKey)
			for index := range event.Options {
				event.Options[index] = redactKey(event.Options[index], request.APIKey)
			}
			clear(message.Payload)
			seen[message.ID] = true
			switch event.Kind {
			case "admit":
				sequence++
				if event.Sequence != sequence || sequence > request.MaxIterations {
					w.fail(ErrProtocol)
					return Result{}, errors.Join(ErrProtocol, w.Close())
				}
			case "question":
				questions++
				if questions > 4 {
					w.fail(ErrProtocol)
					return Result{}, errors.Join(ErrProtocol, w.Close())
				}
			case "progress":
				progress++
				if progress > 2*request.MaxIterations+2 || event.Tool == "browser_exec" && event.Sequence != sequence {
					w.fail(ErrProtocol)
					return Result{}, errors.Join(ErrProtocol, w.Close())
				}
			}
			var reply Reply
			var handlerErr error
			if handler != nil {
				reply, handlerErr = handler(active, event)
			} else if event.Kind != "progress" {
				handlerErr = errHandler
			}
			if active.Err() != nil {
				return Result{}, w.cancelRun(contextError(active), identifier, frames, nil)
			}
			if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
				cause := context.Canceled
				if errors.Is(handlerErr, context.DeadlineExceeded) {
					cause = context.DeadlineExceeded
				}
				return Result{}, w.cancelRun(errors.Join(errHandler, cause), identifier, frames, nil)
			}
			if handlerErr != nil || event.Kind != "progress" && (!validReply(reply, event, request.APIKey) || event.Kind == "admit" && !reply.Approved) {
				refuse, finish := context.WithTimeout(context.Background(), time.Second)
				_ = w.send(refuse, "reply", message.ID, Reply{})
				finish()
				return Result{}, errors.Join(errHandler, contextError(active), w.Close())
			}
			if event.Kind != "progress" {
				if err := w.send(active, "reply", message.ID, reply); err != nil {
					if !errors.Is(err, ErrProtocol) && active.Err() != nil {
						return Result{}, w.cancelRun(contextError(active), identifier, frames, nil)
					}
					w.fail(ErrProtocol)
					return Result{}, errors.Join(ErrProtocol, contextError(active), w.Close())
				}
			}
		}
	}
}

// A non-uncertain result is the child bridge's receipt that this execution has
// stopped and joined its work. Without that receipt the transport is lost;
// cancellation never authorizes replacement or replay of a browser operation.
func (w *Worker) cancelRun(cause error, identifier string, frames <-chan frame, first *frame) error {
	if w.lifetime.Err() != nil {
		return errors.Join(cause, contextError(w.lifetime), w.Close())
	}
	quiesce, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := w.send(quiesce, "cancel", identifier, struct{}{}); err != nil {
		w.fail(ErrUncertain)
		return errors.Join(cause, ErrLost, ErrUncertain, w.Close())
	}
	seen := map[string]bool{}
	for count := 0; count < 128; count++ {
		var message frame
		if first != nil {
			message, first = *first, nil
		} else {
			select {
			case message = <-frames:
			case <-w.lifetime.Done():
				return errors.Join(cause, contextError(w.lifetime), w.Close())
			case <-quiesce.Done():
				w.fail(ErrUncertain)
				return errors.Join(cause, ErrLost, ErrUncertain, w.Close())
			}
		}
		if message.Kind == "result" {
			var result Result
			valid := strictJSON(message.Payload, &result) == nil && validResult(result)
			clear(message.Payload)
			if !valid {
				w.fail(ErrProtocol)
				return errors.Join(cause, ErrProtocol, ErrLost, ErrUncertain, w.Close())
			}
			if result.Status != "uncertain" && w.lifetime.Err() == nil {
				return cause
			}
			w.fail(ErrUncertain)
			return errors.Join(cause, ErrLost, ErrUncertain, w.Close())
		}
		var event Event
		valid := !seen[message.ID] && strictJSON(message.Payload, &event) == nil && validEvent(event)
		clear(message.Payload)
		if !valid {
			w.fail(ErrProtocol)
			return errors.Join(cause, ErrProtocol, ErrLost, ErrUncertain, w.Close())
		}
		seen[message.ID] = true
		if event.Kind != "progress" {
			if err := w.send(quiesce, "reply", message.ID, Reply{}); err != nil {
				w.fail(ErrUncertain)
				return errors.Join(cause, ErrLost, ErrUncertain, w.Close())
			}
		}
	}
	w.fail(ErrProtocol)
	return errors.Join(cause, ErrProtocol, ErrLost, ErrUncertain, w.Close())
}

func validRequest(request Request) bool {
	if !boundedText(request.Objective, 16384, true) || request.Provider != "openai" ||
		!boundedText(request.Model, 200, true) || strings.IndexFunc(request.Model, unicode.IsControl) >= 0 ||
		len(request.APIKey) == 0 || len(request.APIKey) > 8192 || request.MaxIterations < 1 || request.MaxIterations > 30 ||
		request.TimeoutSeconds < 1 || request.TimeoutSeconds > 600 || request.MaxTokens < 1 || request.MaxTokens > 16384 ||
		!boundedText(request.BaseURL, 2048, true) {
		return false
	}
	if !utf8.Valid(request.APIKey) || bytes.ContainsAny(request.APIKey, "\r\n\x00") {
		return false
	}
	if containsKey(request.Objective, request.APIKey) || containsKey(request.Model, request.APIKey) || containsKey(request.BaseURL, request.APIKey) {
		return false
	}
	endpoint, err := url.Parse(request.BaseURL)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || strings.Contains(request.BaseURL, "#") || endpoint.Opaque != "" {
		return false
	}
	if endpoint.Scheme == "https" {
		return true
	}
	if endpoint.Scheme != "http" {
		return false
	}
	return strings.EqualFold(endpoint.Hostname(), "localhost") || net.ParseIP(endpoint.Hostname()).IsLoopback()
}

func boundedText(value string, maximum int, required bool) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0) &&
		(!required || strings.TrimSpace(value) != "")
}

func containsKey(value string, key []byte) bool { return bytes.Contains([]byte(value), key) }

func redactKey(value string, key []byte) string {
	return strings.ReplaceAll(value, string(key), "[REDACTED]")
}

func validEvent(event Event) bool {
	if len(event.Options) > 4 {
		return false
	}
	switch event.Kind {
	case "admit":
		return event.Tool == "browser_exec" && event.Sequence > 0 && event.Text == "" && len(event.Options) == 0
	case "question":
		if event.Tool != "" || event.Sequence != 0 || !boundedText(event.Text, 2000, true) {
			return false
		}
		for _, option := range event.Options {
			if !boundedText(option, 500, true) {
				return false
			}
		}
		return true
	case "progress":
		return (event.Tool == "" || event.Tool == "browser_exec") && (event.Text == "started" || event.Text == "finished") &&
			(event.Tool == "" && event.Sequence == 0 || event.Tool == "browser_exec" && event.Sequence > 0) && len(event.Options) == 0
	}
	return false
}

func validReply(reply Reply, event Event, key []byte) bool {
	if !boundedText(reply.Answer, 2000, event.Kind == "question") || containsKey(reply.Answer, key) {
		return false
	}
	return event.Kind == "question" || reply.Answer == ""
}

func validResult(result Result) bool {
	if !boundedText(result.Text, 65536, false) || result.ModelRequests < 0 || result.ModelRequests > 1_000_000 {
		return false
	}
	switch result.Status {
	case "completed", "incomplete", "interrupted", "failed", "uncertain":
		return true
	}
	return false
}

func contextError(ctx context.Context) error {
	return errors.Join(ctx.Err(), context.Cause(ctx))
}

// Close interrupts a pending execution, joins shutdown and only removes private
// state after an explicit clean receipt and a successful process exit.
func (w *Worker) Close() error {
	w.closeOnce.Do(func() { go w.shutdown() })
	<-w.closeDone
	return w.closeErr
}

func (w *Worker) shutdown() {
	defer close(w.closeDone)
	identifier, idErr := freshID()
	w.mu.Lock()
	w.closing = true
	w.closeID = identifier
	w.mu.Unlock()
	w.cancel(ErrClosed)
	grace, finish := context.WithTimeout(context.Background(), 10*time.Second)
	defer finish()
	if idErr == nil {
		_ = w.send(grace, "close", identifier, struct{}{})
	}
	acknowledged, exited := false, false
	ack, process := w.closedAck, w.procDone
	for !(acknowledged && exited) {
		select {
		case <-ack:
			acknowledged = true
			ack = nil
		case <-process:
			exited = true
			process = nil
			if !acknowledged {
				select {
				case <-w.closedAck:
					acknowledged = true
				case <-w.readDone:
				case <-grace.Done():
				}
			}
		case <-grace.Done():
		}
		if grace.Err() != nil || exited && !acknowledged {
			break
		}
	}
	w.mu.Lock()
	clean := acknowledged && exited && w.clean && w.waitErr == nil && !w.fault
	w.mu.Unlock()
	if clean && processGroupExists(w.cmd) {
		clean = false
	}
	if !clean {
		killProcessGroup(w.cmd)
		select {
		case <-w.procDone:
		case <-time.After(5 * time.Second):
		}
	}
	_ = w.input.Close()
	if clean {
		select {
		case <-w.readDone:
		case <-time.After(time.Second):
			clean = false
		}
	}
	_ = w.output.Close()
	<-w.readDone
	w.mu.Lock()
	clean = clean && !w.fault
	w.mu.Unlock()
	if clean {
		if os.RemoveAll(w.work) != nil {
			w.closeErr = ErrUncertain
		}
	} else {
		w.closeErr = ErrUncertain
	}
}
