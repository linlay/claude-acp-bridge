package acpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/acp-go-sdk"

	"proxy-acp/internal/platform"
)

type turn struct {
	req  platform.QueryRequest
	sink EventSink
	mgr  *Manager

	seq atomic.Int64

	mu            sync.Mutex
	contentOpen   bool
	contentID     string
	reasoningOpen bool
	reasoningID   string
	planSeen      bool
	toolStarted   map[string]bool
	toolEnded     map[string]bool
}

type pendingPermission struct {
	runID      string
	awaitingID string
	options    []acp.PermissionOption
	response   chan acp.RequestPermissionOutcome
	done       chan struct{}
}

func newTurn(req platform.QueryRequest, sink EventSink, mgr *Manager) *turn {
	return &turn{
		req:         req,
		sink:        sink,
		mgr:         mgr,
		contentID:   stableID(req.RunID, "content"),
		reasoningID: stableID(req.RunID, "reasoning"),
		toolStarted: map[string]bool{},
		toolEnded:   map[string]bool{},
	}
}

func (t *turn) emit(eventType string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	event := platform.NewEvent(t.seq.Add(1), eventType, payload)
	if err := t.sink.Publish(event); err != nil {
		// The HTTP/WS caller owns connection cancellation; publishing errors
		// are intentionally not fatal inside ACP callbacks.
		return
	}
}

func (t *turn) emitRunError(err error) {
	if err == nil {
		return
	}
	t.emit("run.error", map[string]any{
		"runId": t.req.RunID,
		"error": map[string]any{
			"code":    "acp_error",
			"message": err.Error(),
		},
	})
}

func (t *turn) handleUpdate(update acp.SessionUpdate) error {
	switch {
	case update.AgentMessageChunk != nil:
		text := contentBlockText(update.AgentMessageChunk.Content)
		if text != "" {
			t.ensureContent()
			t.emit("content.delta", map[string]any{"contentId": t.contentID, "delta": text})
		}
	case update.AgentThoughtChunk != nil:
		text := contentBlockText(update.AgentThoughtChunk.Content)
		if text != "" {
			t.ensureReasoning()
			t.emit("reasoning.delta", map[string]any{"reasoningId": t.reasoningID, "delta": text})
		}
	case update.ToolCall != nil:
		t.handleToolStart(*update.ToolCall)
	case update.ToolCallUpdate != nil:
		t.handleToolUpdate(*update.ToolCallUpdate)
	case update.Plan != nil:
		eventType := "plan.create"
		if t.planSeen {
			eventType = "plan.update"
		}
		t.planSeen = true
		t.emit(eventType, map[string]any{
			"planId": t.req.RunID + "_plan",
			"chatId": t.req.ChatID,
			"plan":   planPayload(update.Plan.Entries),
		})
	case update.UsageUpdate != nil:
		t.emit("debug.postCall", map[string]any{
			"runId":  t.req.RunID,
			"chatId": t.req.ChatID,
			"data":   map[string]any{"contextWindow": update.UsageUpdate.Size, "currentContextSize": update.UsageUpdate.Used},
		})
	}
	return nil
}

func (t *turn) ensureContent() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.contentOpen {
		return
	}
	t.contentOpen = true
	t.emit("content.start", map[string]any{"contentId": t.contentID, "runId": t.req.RunID})
}

func (t *turn) ensureReasoning() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reasoningOpen {
		return
	}
	t.reasoningOpen = true
	t.emit("reasoning.start", map[string]any{"reasoningId": t.reasoningID, "runId": t.req.RunID})
}

func (t *turn) closeOpenStreams() {
	t.mu.Lock()
	contentOpen := t.contentOpen
	reasoningOpen := t.reasoningOpen
	t.contentOpen = false
	t.reasoningOpen = false
	t.mu.Unlock()
	if reasoningOpen {
		t.emit("reasoning.end", map[string]any{"reasoningId": t.reasoningID})
	}
	if contentOpen {
		t.emit("content.end", map[string]any{"contentId": t.contentID})
	}
	t.closeOpenTools()
}

func (t *turn) handleToolStart(tool acp.SessionUpdateToolCall) {
	toolID := string(tool.ToolCallId)
	if toolID == "" {
		return
	}
	t.mu.Lock()
	already := t.toolStarted[toolID]
	if !already {
		t.toolStarted[toolID] = true
	}
	t.mu.Unlock()
	if already {
		return
	}
	t.emit("tool.start", map[string]any{
		"toolId":          toolID,
		"runId":           t.req.RunID,
		"toolName":        toolName(tool.Title, tool.Kind),
		"toolLabel":       tool.Title,
		"toolDescription": tool.Title,
	})
	if tool.RawInput != nil {
		t.emit("tool.args", map[string]any{"toolId": toolID, "delta": marshalAny(tool.RawInput), "chunkIndex": 0})
	}
	t.endTool(toolID)
	if tool.Status == acp.ToolCallStatusCompleted || tool.Status == acp.ToolCallStatusFailed {
		t.emitToolResult(toolID, tool.RawOutput, tool.Status)
	}
}

func (t *turn) handleToolUpdate(update acp.SessionToolCallUpdate) {
	toolID := string(update.ToolCallId)
	if toolID == "" {
		return
	}
	t.mu.Lock()
	started := t.toolStarted[toolID]
	t.mu.Unlock()
	if !started {
		title := toolID
		if update.Title != nil {
			title = *update.Title
		}
		kind := acp.ToolKindOther
		if update.Kind != nil {
			kind = *update.Kind
		}
		t.handleToolStart(acp.SessionUpdateToolCall{ToolCallId: update.ToolCallId, Title: title, Kind: kind, RawInput: update.RawInput})
	}
	if update.RawInput != nil {
		t.emit("tool.args", map[string]any{"toolId": toolID, "delta": marshalAny(update.RawInput), "chunkIndex": 1})
	}
	t.endTool(toolID)
	if update.Status != nil && (*update.Status == acp.ToolCallStatusCompleted || *update.Status == acp.ToolCallStatusFailed) {
		result := update.RawOutput
		if result == nil && len(update.Content) > 0 {
			result = toolContentPayload(update.Content)
		}
		t.emitToolResult(toolID, result, *update.Status)
	}
}

func (t *turn) endTool(toolID string) {
	t.mu.Lock()
	if t.toolEnded[toolID] {
		t.mu.Unlock()
		return
	}
	t.toolEnded[toolID] = true
	t.mu.Unlock()
	t.emit("tool.end", map[string]any{"toolId": toolID})
}

func (t *turn) emitToolResult(toolID string, result any, status acp.ToolCallStatus) {
	payload := map[string]any{"toolId": toolID, "result": result}
	if status == acp.ToolCallStatusFailed {
		payload["error"] = "ACP tool call failed"
		payload["exitCode"] = 1
	}
	t.emit("tool.result", payload)
}

func (t *turn) closeOpenTools() {
	t.mu.Lock()
	ids := make([]string, 0, len(t.toolStarted))
	for id := range t.toolStarted {
		if !t.toolEnded[id] {
			ids = append(ids, id)
			t.toolEnded[id] = true
		}
	}
	t.mu.Unlock()
	for _, id := range ids {
		t.emit("tool.end", map[string]any{"toolId": id})
	}
}

func (t *turn) requestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	awaitingID := stableID(t.req.RunID, "perm_"+string(params.ToolCall.ToolCallId))
	pending := &pendingPermission{
		runID:      t.req.RunID,
		awaitingID: awaitingID,
		options:    params.Options,
		response:   make(chan acp.RequestPermissionOutcome, 1),
		done:       make(chan struct{}),
	}
	t.mgr.registerAwaiting(pending)
	defer func() {
		close(pending.done)
		t.mgr.unregisterAwaiting(awaitingID, pending)
	}()

	approvals := make([]map[string]any, 0, len(params.Options))
	for _, option := range params.Options {
		approvals = append(approvals, map[string]any{
			"id":          string(option.OptionId),
			"command":     permissionCommand(params.ToolCall),
			"description": option.Name,
			"options": []map[string]any{
				{"label": option.Name, "decision": decisionForOption(option.Kind)},
				{"label": "Reject", "decision": "reject"},
			},
			"allowFreeText": true,
		})
	}
	t.emit("awaiting.ask", map[string]any{
		"awaitingId":   awaitingID,
		"mode":         "approval",
		"viewportType": "builtin",
		"viewportKey":  "approval",
		"timeout":      int64(120000),
		"runId":        t.req.RunID,
		"approvals":    approvals,
	})

	select {
	case outcome := <-pending.response:
		t.emit("request.submit", map[string]any{"runId": t.req.RunID, "chatId": t.req.ChatID, "awaitingId": awaitingID, "params": outcomePayload(outcome)})
		t.emit("awaiting.answer", map[string]any{"awaitingId": awaitingID, "mode": "approval", "status": "answered", "approvals": outcomePayload(outcome)})
		return acp.RequestPermissionResponse{Outcome: outcome}, nil
	case <-ctx.Done():
		outcome := acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}
		t.emit("awaiting.answer", map[string]any{
			"awaitingId": awaitingID,
			"mode":       "approval",
			"status":     "error",
			"error":      map[string]any{"code": "timeout", "message": ctx.Err().Error()},
		})
		return acp.RequestPermissionResponse{Outcome: outcome}, nil
	}
}

func contentBlockText(block acp.ContentBlock) string {
	switch {
	case block.Text != nil:
		return block.Text.Text
	case block.ResourceLink != nil:
		return block.ResourceLink.Uri
	case block.Resource != nil:
		data, _ := json.Marshal(block.Resource.Resource)
		return string(data)
	default:
		data, _ := json.Marshal(block)
		return string(data)
	}
}

func toolContentPayload(items []acp.ToolCallContent) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		switch {
		case item.Content != nil:
			out = append(out, contentBlockText(item.Content.Content))
		case item.Diff != nil:
			out = append(out, map[string]any{"path": item.Diff.Path, "oldText": item.Diff.OldText, "newText": item.Diff.NewText})
		case item.Terminal != nil:
			out = append(out, map[string]any{"terminalId": item.Terminal.TerminalId})
		}
	}
	return out
}

func planPayload(entries []acp.PlanEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for idx, entry := range entries {
		out = append(out, map[string]any{
			"id":       fmt.Sprintf("plan_%d", idx+1),
			"content":  entry.Content,
			"priority": string(entry.Priority),
			"status":   string(entry.Status),
		})
	}
	return out
}

func toolName(title string, kind acp.ToolKind) string {
	if kind != "" {
		return string(kind)
	}
	title = strings.TrimSpace(strings.ToLower(title))
	if title == "" {
		return "tool"
	}
	return strings.ReplaceAll(title, " ", "_")
}

func stableID(runID string, suffix string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		runID = "run"
	}
	return runID + "_" + suffix
}

func permissionCommand(update acp.ToolCallUpdate) string {
	if update.RawInput != nil {
		return marshalAny(update.RawInput)
	}
	if update.Title != nil {
		return *update.Title
	}
	return string(update.ToolCallId)
}

func decisionForOption(kind acp.PermissionOptionKind) string {
	switch kind {
	case acp.PermissionOptionKindAllowAlways:
		return "approve_rule_run"
	case acp.PermissionOptionKindAllowOnce:
		return "approve"
	default:
		return "reject"
	}
}

func permissionOutcomeFromSubmit(params []map[string]any, options []acp.PermissionOption) acp.RequestPermissionOutcome {
	if len(params) == 0 {
		return acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}
	}
	decision, _ := params[0]["decision"].(string)
	id, _ := params[0]["id"].(string)
	if strings.HasPrefix(strings.ToLower(decision), "reject") {
		return acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}
	}
	if id == "" {
		for _, option := range options {
			if option.Kind == acp.PermissionOptionKindAllowOnce || option.Kind == acp.PermissionOptionKindAllowAlways {
				id = string(option.OptionId)
				break
			}
		}
	}
	if id == "" && len(options) > 0 {
		id = string(options[0].OptionId)
	}
	if id == "" {
		return acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}
	}
	return acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: acp.PermissionOptionId(id)}}
}

func outcomePayload(outcome acp.RequestPermissionOutcome) []map[string]any {
	if outcome.Cancelled != nil {
		return []map[string]any{{"decision": "reject"}}
	}
	if outcome.Selected != nil {
		return []map[string]any{{"id": string(outcome.Selected.OptionId), "decision": "approve"}}
	}
	return []map[string]any{{"decision": "reject"}}
}
