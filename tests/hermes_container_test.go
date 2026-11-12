package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/hermes/containers"
	"github.com/defos-ai/commander/framework/id"
)

func TestHermesContainerRetainsUnconfirmedPrivateHomeCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Docker launcher requires a Unix socket")
	}
	const private = "fixture-private-daemon-error"
	container, volume := strings.Repeat("a", 64), strings.Repeat("c", 64)
	image := "sha256:" + strings.Repeat("b", 64)
	var mu sync.Mutex
	var labels map[string]string
	var attached net.Conn
	containerGone, volumeGone, allowDelete, removeData := false, false, false, false
	containerDeletes, volumeDeletes, staleInspects := 0, 0, 0
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "docker.sock"))
	if err != nil {
		t.Fatal("cannot open the owned Docker API fixture")
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			response = map[string]string{"ApiVersion": "1.47", "MinAPIVersion": "1.24"}
		case r.Method == http.MethodGet && r.URL.Path == "/v1.47/images/"+image+"/json":
			response = map[string]any{"Id": image, "Config": map[string]any{}}
		case r.Method == http.MethodPost && r.URL.Path == "/v1.47/containers/create":
			var create struct{ Labels map[string]string }
			if json.NewDecoder(r.Body).Decode(&create) != nil {
				t.Error("the launcher sent an invalid owned create")
			}
			labels = create.Labels
			w.WriteHeader(http.StatusCreated)
			response = map[string]any{"Id": container, "Warnings": []string{}}
		case r.Method == http.MethodGet && r.URL.Path == "/v1.47/containers/"+container+"/json":
			if containerGone {
				staleInspects++
				http.Error(w, private, http.StatusNotFound)
				return
			}
			response = map[string]any{"Id": container, "Image": image, "Config": map[string]any{"Labels": labels},
				"State":  map[string]any{"Running": false, "Status": "exited"},
				"Mounts": []any{map[string]any{"Type": "volume", "Name": volume, "Driver": "local", "Destination": "/home/commander", "RW": true}}}
		case r.Method == http.MethodPost && r.URL.Path == "/v1.47/containers/"+container+"/attach":
			connection, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error("cannot attach the owned Docker stream fixture")
				return
			}
			attached = connection
			_, _ = buffer.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buffer.Flush()
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v1.47/containers/"+container+"/start":
			w.WriteHeader(http.StatusNoContent)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v1.47/containers/"+container+"/wait":
			response = map[string]any{"StatusCode": 0}
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.47/containers/"+container:
			containerDeletes++
			containerGone = true // Docker can acknowledge this while the home remains.
			w.WriteHeader(http.StatusNoContent)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/v1.47/volumes/"+volume:
			if volumeGone {
				http.NotFound(w, r)
				return
			}
			response = map[string]string{"Name": volume, "Driver": "local", "CreatedAt": "2026-01-01T00:00:00Z"}
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.47/volumes/"+volume:
			volumeDeletes++
			if !allowDelete {
				http.Error(w, private, http.StatusConflict)
				return
			}
			volumeGone = removeData
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			t.Error("the launcher reached an unowned Docker API operation")
			http.Error(w, private, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		mu.Lock()
		if attached != nil {
			_ = attached.Close()
		}
		mu.Unlock()
		_ = server.Close()
		<-served
	})
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	worker, err := containers.Launch(ctx, containers.Config{Socket: listener.Addr().String(), Image: image, Session: id.New()})
	if err != nil || worker == nil {
		t.Fatal("the Docker cleanup fixture did not launch")
	}
	defer worker.CloseIO()
	if status, err := worker.Wait(ctx); err != nil || status != 0 {
		t.Fatal("the Docker fixture did not confirm its stopped receipt")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := worker.Remove(ctx); !errors.Is(err, containers.ErrUncertain) || strings.Contains(err.Error(), private) || worker.ID() != container {
			t.Fatal("unconfirmed private-home cleanup was accepted or exposed daemon output")
		}
		mu.Lock()
		allowDelete = true // The next receipt succeeds while its volume is still present.
		mu.Unlock()
	}
	mu.Lock()
	removeData = true
	mu.Unlock()
	if worker.Remove(ctx) != nil || worker.Remove(ctx) != nil {
		t.Fatal("owned private-home cleanup did not recover after confirmed removal")
	}
	mu.Lock()
	defer mu.Unlock()
	if !volumeGone || containerDeletes != 1 || volumeDeletes != 3 || staleInspects != 0 {
		t.Fatal("partial cleanup retried a removed container or lost its pinned home")
	}
}
