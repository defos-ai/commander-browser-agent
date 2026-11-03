//go:build browserintegration

package browser_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/executions"
)

type checkpointPeerKey struct {
	AgentID   string
	CommandID string
}

// The protocol peer anchors an execution's work on its first checkpoint request.
// Real controller admission/claims are outside this harness; immutable fact
// validation and ownership across subsequent requests use framework contracts.
func (p *browserSessionPeer) handleCheckpoint(message messages.Message) bool {
	if message.Type != controllerevents.CheckpointRequested {
		return false
	}
	request, err := controllerevents.DecodeCheckpointRequest(message)
	if err != nil {
		p.t.Fatal("invalid framework checkpoint request")
	}
	// Tests can intercept requests before persistence, or call checkpointResponse
	// here and omit its reply to simulate a persisted write whose reply is lost.
	if p.checkpointHook != nil && p.checkpointHook(cloneCheckpointMessage(message), request) {
		return true
	}
	p.replyCheckpoint(message, p.checkpointResponse(message))
	return true
}

func (p *browserSessionPeer) checkpointResponse(message messages.Message) agentevents.CheckpointResponse {
	p.t.Helper()
	request, err := controllerevents.DecodeCheckpointRequest(message)
	if err != nil {
		p.t.Fatal("invalid framework checkpoint request")
	}
	response := agentevents.CheckpointResponse{CommandID: request.CommandID, Name: request.Name}
	key := checkpointPeerKey{AgentID: message.AgentID, CommandID: request.CommandID}
	if p.checkpoints == nil {
		p.checkpoints = make(map[checkpointPeerKey]executions.Execution)
	}
	record, found := p.checkpoints[key]
	if !found {
		claim, err := agentcommands.NewRunAction(message.AgentID, agentcommands.ActionCall{
			ActionName: "checkpoint-test", Input: json.RawMessage(`{}`), Work: message.Work,
		})
		if err != nil {
			p.t.Fatal("cannot establish checkpoint peer execution")
		}
		claim.ID = request.CommandID
		record = executions.Execution{Command: claim, StartedAt: time.Now().UTC()}
	} else if record.Command.Work != message.Work {
		response.Error = agentevents.CheckpointInvalid
		return response
	}
	if request.Operation == controllerevents.CheckpointSave {
		if _, err := record.RecordCheckpoint(request.Name, request.Data); err != nil {
			switch {
			case errors.Is(err, executions.ErrCheckpointConflict):
				response.Error = agentevents.CheckpointConflict
			case errors.Is(err, executions.ErrExecutionCompleted):
				response.Error = agentevents.CheckpointCompleted
			default:
				response.Error = agentevents.CheckpointInvalid
			}
			return response
		}
	}
	p.checkpoints[key] = record
	response.Data = bytes.Clone(record.Checkpoints[request.Name])
	if len(response.Data) == 0 {
		response.Error = agentevents.CheckpointNotFound
	}
	return response
}

func (p *browserSessionPeer) replyCheckpoint(request messages.Message, response agentevents.CheckpointResponse) {
	p.t.Helper()
	message, err := agentevents.NewCheckpointResponse(request, response)
	if err != nil {
		p.t.Fatal("invalid checkpoint peer response")
	}
	if err := (agentevents.Client{Bus: p.bus}).Publish(p.t.Context(), message); err != nil {
		p.t.Fatal("cannot publish checkpoint peer response")
	}
}

func (p *browserSessionPeer) checkpointFacts(command messages.Message) map[string]json.RawMessage {
	p.t.Helper()
	record, found := p.checkpoints[checkpointPeerKey{AgentID: command.AgentID, CommandID: command.ID}]
	if !found {
		return map[string]json.RawMessage{}
	}
	if record.Command.Work != command.Work {
		p.t.Fatal("checkpoint facts do not belong to the requested work")
	}
	facts := make(map[string]json.RawMessage, len(record.Checkpoints))
	for name, data := range record.Checkpoints {
		facts[name] = bytes.Clone(data)
	}
	return facts
}

func (p *browserSessionPeer) completeCheckpoints(result messages.Message) {
	key := checkpointPeerKey{AgentID: result.AgentID, CommandID: result.CorrelationID}
	record, found := p.checkpoints[key]
	if !found {
		return
	}
	if record.Command.Work != result.Work {
		p.t.Fatal("result does not belong to its checkpoint execution")
	}
	copy := cloneCheckpointMessage(result)
	now := time.Now().UTC()
	record.Result, record.CompletedAt = &copy, &now
	p.checkpoints[key] = record
}

func cloneCheckpointMessage(message messages.Message) messages.Message {
	message.Payload = bytes.Clone(message.Payload)
	return message
}
