package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	browser "github.com/defos-ai/commander-browser-agent"
	"github.com/defos-ai/commander-browser-agent/cli"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/comms"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	fscomms "github.com/defos-ai/commander/providers/fs/comms"
)

func TestCLIAgentIDSuffixRegistration(t *testing.T) {
	for _, test := range []struct {
		name, suffix, expected string
		flags                  []string
	}{
		{name: "manual"},
		{name: "empty"},
		{name: "environment", suffix: "0123abcd", expected: "0123abcd"},
		{name: "flag", flags: []string{"--agent-id-suffix=89abcdef"}, expected: "89abcdef"},
		{name: "flag overrides environment", suffix: "invalid", flags: []string{"--agent-id-suffix=89abcdef"}, expected: "89abcdef"},
		{name: "empty flag overrides environment", suffix: "0123abcd", flags: []string{"--agent-id-suffix="}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("COMMANDER_AGENT_ID_SUFFIX", test.suffix)
			if test.name == "manual" {
				if err := os.Unsetenv("COMMANDER_AGENT_ID_SUFFIX"); err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			bus, err := fscomms.NewFsBus(filepath.Join(root, "bus"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := bus.Close(); err != nil {
					t.Errorf("close observer bus: %v", err)
				}
			})
			subscription, err := bus.Subscribe(t.Context(), comms.Inbox{
				Topic: messages.ControllerEvents(messages.DefaultControllerType),
				Name:  "registration-test", Mode: comms.TransientExclusive,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := subscription.Close(); err != nil {
					t.Errorf("close registration observer: %v", err)
				}
			})
			args := []string{
				"--bus-url=" + filepath.Join(root, "bus"),
				"--storage-url=" + filepath.Join(root, "storage"),
				"--log-level=error", "--log-format=json", "--actions=",
			}
			args = append(args, test.flags...)
			first := observeCLIRegistration(t, args, subscription)
			snapshots := []agent.AgentSnapshot{first}
			if test.expected == "" {
				second := observeCLIRegistration(t, args, subscription)
				if first.ID == second.ID {
					t.Fatal("manual restart reused the previous process's agent ID")
				}
				snapshots = append(snapshots, second)
			}
			for _, snapshot := range snapshots {
				if snapshot.Type != browser.AgentType || snapshot.Status != agent.AgentReady || !strings.HasPrefix(snapshot.ID, "browser-agent-") {
					t.Fatalf("unexpected registration: %+v", snapshot)
				}
				var config map[string]string
				if err := json.Unmarshal(snapshot.Config, &config); err != nil {
					t.Fatalf("decode public config: %v", err)
				}
				if len(config) != 0 {
					t.Fatalf("unexpected public configuration: %s", snapshot.Config)
				}
				if want := "browser-agent-" + test.expected; test.expected != "" && snapshot.ID != want {
					t.Fatalf("registered ID = %q, want preassigned %q", snapshot.ID, want)
				}
			}
		})
	}
}

func TestCLIRejectsInvalidAgentIDSuffixBeforeOpeningProviders(t *testing.T) {
	for _, suffix := range []string{
		" ", "0123ABCD", "123", "0123abc", "0123abcde", "0123abcg",
		"browser-agent-0123abcd", "other-agent-0123abcd", "../0123abcd", "0123abcd\n",
	} {
		t.Run(suffix, func(t *testing.T) {
			t.Setenv("COMMANDER_AGENT_ID_SUFFIX", suffix)
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			args := []string{"--bus-url=" + filepath.Join(root, "bus"), "--storage-url=" + filepath.Join(root, "storage")}
			err := cli.Run(ctx, args, nil)
			if flagErr := cli.Run(ctx, append(args, "--agent-id-suffix="+suffix), nil); flagErr == nil || !strings.Contains(flagErr.Error(), "exactly 8 lowercase hex characters") {
				t.Fatalf("expected invalid flag error, got %v", flagErr)
			}
			if err == nil || !strings.Contains(err.Error(), "exactly 8 lowercase hex characters") {
				t.Fatalf("expected invalid agent ID suffix error, got %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid suffix opened providers: entries=%v, err=%v", entries, err)
			}
		})
	}
}

func observeCLIRegistration(t *testing.T, args []string, subscription comms.Subscription) agent.AgentSnapshot {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = cli.Run(ctx, args, nil)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("stop agent: %v", runErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop after cancellation")
		}
	}
	t.Cleanup(stop)
	started := receiveAgentSnapshot(t, subscription, controllerevents.AgentStarted)
	stop()
	stopped := receiveAgentSnapshot(t, subscription, controllerevents.AgentStopped)
	if stopped.ID != started.ID || stopped.Status != agent.AgentStopped || string(stopped.Config) != string(started.Config) {
		t.Fatalf("shutdown did not preserve the instance's registration: started=%+v stopped=%+v", started, stopped)
	}
	return started
}

func receiveAgentSnapshot(t *testing.T, subscription comms.Subscription, kind messages.MessageType) agent.AgentSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		delivery, err := subscription.Next(ctx)
		if err != nil {
			t.Fatalf("receive %s: %v", kind, err)
		}
		message := delivery.Message()
		if err := delivery.Ack(ctx); err != nil {
			t.Fatalf("acknowledge registration: %v", err)
		}
		if message.Type != kind {
			continue
		}
		var snapshot agent.AgentSnapshot
		if err := json.Unmarshal(message.Payload, &snapshot); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
		return snapshot
	}
}
