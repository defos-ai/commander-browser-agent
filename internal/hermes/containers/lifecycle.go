package containers

import (
	"context"
	"encoding/json"
	"net/http"
)

type containerState struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Driver      string `json:"Driver"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	State struct {
		Running *bool  `json:"Running"`
		Paused  bool   `json:"Paused"`
		Status  string `json:"Status"`
	} `json:"State"`
}

// inspect also resolves a pending named create, but never adopts a foreign
// resource which happens to have the same name or a different immutable image.
func (instance *Instance) inspect(ctx context.Context) (containerState, error) {
	reference := instance.ID()
	response, err := instance.request(ctx, http.MethodGet, apiPrefix+"/containers/"+reference+"/json", nil)
	var state containerState
	if err != nil || response.status != http.StatusOK || json.Unmarshal(response.body, &state) != nil ||
		!containerID(state.ID) || containerID(reference) && state.ID != reference || state.Image != instance.config.Image || state.State.Running == nil ||
		state.Config.Labels[labelType] != "hermes" || state.Config.Labels[labelSession] != instance.config.Session ||
		state.Config.Labels[labelOwner] != instance.nonce {
		return containerState{}, operationError(ctx, ErrUncertain)
	}
	volume := ""
	for _, mount := range state.Mounts {
		if mount.Type == "tmpfs" && mount.Destination == "/tmp" && mount.RW {
			continue
		}
		if mount.Type != "volume" || mount.Driver != "local" || mount.Destination != "/home/commander" || !mount.RW || !containerID(mount.Name) || volume != "" {
			return containerState{}, operationError(ctx, ErrUncertain)
		}
		volume = mount.Name
	}
	if volume == "" {
		return containerState{}, operationError(ctx, ErrUncertain)
	}
	created, absent, err := instance.inspectVolume(ctx, volume)
	if err != nil || absent {
		return containerState{}, operationError(ctx, ErrUncertain)
	}
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if instance.volume != "" && (instance.volume != volume || instance.volumeCreated != created) {
		return containerState{}, operationError(ctx, ErrUncertain)
	}
	instance.ref = state.ID
	instance.volume = volume
	instance.volumeCreated = created
	return state, nil
}

// Wait confirms Docker's not-running receipt, not a clean worker shutdown or
// whether a created container ever started. Caller cancellation bounds the wait.
func (instance *Instance) Wait(ctx context.Context) (int, error) {
	if _, err := instance.inspect(ctx); err != nil {
		return 0, err
	}
	response, err := instance.request(ctx, http.MethodPost, apiPrefix+"/containers/"+instance.ID()+"/wait?condition=not-running", nil)
	var receipt struct {
		Status *int `json:"StatusCode"`
		Error  *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err != nil || response.status != http.StatusOK || json.Unmarshal(response.body, &receipt) != nil ||
		receipt.Status == nil || *receipt.Status < 0 || *receipt.Status > 255 || receipt.Error != nil && receipt.Error.Message != "" {
		return 0, operationError(ctx, ErrUncertain)
	}
	instance.mu.Lock()
	instance.stopped = true
	instance.mu.Unlock()
	return *receipt.Status, nil
}

// Kill only targets this verified owned resource, then waits for a stopped
// receipt. A missing resource or receipt stays uncertain rather than confirmed.
func (instance *Instance) Kill(ctx context.Context) error {
	if _, err := instance.inspect(ctx); err != nil {
		return err
	}
	response, err := instance.request(ctx, http.MethodPost, apiPrefix+"/containers/"+instance.ID()+"/kill?signal=KILL", nil)
	if err != nil || response.status != http.StatusNoContent && response.status != http.StatusConflict {
		return operationError(ctx, ErrUncertain)
	}
	_, err = instance.Wait(ctx)
	return err
}

// Remove requires an earlier stopped receipt and rechecks actual state. Force
// is never used, so Docker refuses removal if another actor restarted it. The
// explicit removal also deletes its sole owned anonymous private-home volume.
func (instance *Instance) Remove(ctx context.Context) error {
	instance.mu.Lock()
	stopped, containerRemoved, removed := instance.stopped, instance.containerRemoved, instance.removed
	instance.mu.Unlock()
	if removed {
		return nil
	}
	if !stopped {
		return operationError(ctx, ErrUncertain)
	}
	if !containerRemoved {
		state, err := instance.inspect(ctx)
		if err != nil || *state.State.Running || state.State.Paused || state.State.Status != "created" && state.State.Status != "exited" && state.State.Status != "dead" {
			return operationError(ctx, ErrUncertain)
		}
		response, err := instance.request(ctx, http.MethodDelete, apiPrefix+"/containers/"+instance.ID()+"?force=false&v=true", nil)
		if err != nil || response.status != http.StatusNoContent {
			return operationError(ctx, ErrUncertain)
		}
		instance.mu.Lock()
		instance.containerRemoved = true
		instance.mu.Unlock()
		_ = instance.CloseIO()
	}
	if err := instance.removeVolume(ctx); err != nil {
		return err
	}
	instance.mu.Lock()
	instance.removed = true
	instance.mu.Unlock()
	return instance.CloseIO()
}

// Container removal can succeed while Docker silently retains its volume.
// Keep that partial state so only the pinned anonymous home can be retried.
func (instance *Instance) removeVolume(ctx context.Context) error {
	instance.mu.Lock()
	volume, expected := instance.volume, instance.volumeCreated
	instance.mu.Unlock()
	if volume == "" || expected == "" {
		return operationError(ctx, ErrUncertain)
	}
	created, absent, err := instance.inspectVolume(ctx, volume)
	if err != nil || !absent && created != expected {
		return operationError(ctx, ErrUncertain)
	}
	if absent {
		return nil
	}
	response, err := instance.request(ctx, http.MethodDelete, apiPrefix+"/volumes/"+volume+"?force=false", nil)
	if err != nil || response.status != http.StatusNoContent && response.status != http.StatusNotFound {
		return operationError(ctx, ErrUncertain)
	}
	_, absent, err = instance.inspectVolume(ctx, volume)
	if err != nil || !absent {
		return operationError(ctx, ErrUncertain)
	}
	return nil
}

func (instance *Instance) inspectVolume(ctx context.Context, name string) (created string, absent bool, err error) {
	response, err := instance.request(ctx, http.MethodGet, apiPrefix+"/volumes/"+name, nil)
	if err == nil && response.status == http.StatusNotFound {
		return "", true, nil
	}
	var volume struct{ Name, Driver, CreatedAt string }
	if err != nil || response.status != http.StatusOK || json.Unmarshal(response.body, &volume) != nil ||
		volume.Name != name || volume.Driver != "local" || volume.CreatedAt == "" || len(volume.CreatedAt) > 128 {
		return "", false, operationError(ctx, ErrUncertain)
	}
	return volume.CreatedAt, false, nil
}

// CloseIO interrupts blocked reads/writes and loses the private protocol receipt.
// StdinOnce closes worker input on detach, but this never implies clean cleanup.
func (instance *Instance) CloseIO() error {
	instance.mu.Lock()
	stream := instance.stream
	instance.mu.Unlock()
	if stream != nil {
		return stream.close()
	}
	return nil
}
