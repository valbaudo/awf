package codexlive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/valbaudo/awf/agent"
)

const (
	codexBinary = "codex"

	serverRequestCommandApproval     = "item/commandExecution/requestApproval"
	serverRequestFileApproval        = "item/fileChange/requestApproval"
	serverRequestPermissionsApproval = "item/permissions/requestApproval"
)

type processClient struct {
	env agent.SecretEnv

	mu           sync.Mutex
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	pending      map[string]chan rpcResponse
	turns        map[string]chan ProviderEvent
	earlyEvents  map[string][]ProviderEvent
	earlyClosed  map[string]bool
	requestKinds map[string]string
	nextID       int64
	started      bool
	closed       bool
	lastStderr   string
}

func newProcessClient(env agent.SecretEnv) *processClient {
	return &processClient{env: env}
}

// loginRunner is the `codex login --with-api-key` invocation — a package var so
// tests substitute a recorder (same seam pattern as the backoff vars).
var loginRunner = func(ctx context.Context, codexPath, apiKey string, env []string) error {
	cmd := exec.CommandContext(ctx, codexPath, "login", "--with-api-key")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(apiKey) // key via stdin, never argv
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("agent/codexlive: codex login --with-api-key: %w: %s", err, firstNonEmptyLine(out))
	}
	return nil
}

// authJSONPath locates codex's auth record: $CODEX_HOME/auth.json, defaulting
// to ~/.codex/auth.json (mirrors the sandbox's credDirs resolution).
func authJSONPath() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".codex")
	}
	return filepath.Join(home, "auth.json")
}

// ensureAPIKeyAuth materializes auth.json from OPENAI_API_KEY when the env
// carries a key and no auth.json exists — codex (verified 0.146.0, 2026-08-16)
// does not honor the bare env var for app-server/exec auth (401 "Missing
// bearer"); it needs the login record. Idempotent; an existing auth.json
// (ChatGPT OAuth) always wins. Same contract as agent/codex's shell prelude,
// but process-spawned: codexlive drives the CLI without a shell.
func (c *processClient) ensureAPIKeyAuth(ctx context.Context, codexPath string) error {
	key := c.env["OPENAI_API_KEY"]
	if key == "" {
		return nil
	}
	if _, err := os.Stat(authJSONPath()); err == nil {
		return nil
	}
	// codex refuses to create CODEX_HOME itself (0.146.0) — mkdir first
	if err := os.MkdirAll(filepath.Dir(authJSONPath()), 0o700); err != nil {
		return fmt.Errorf("agent/codexlive: create codex home: %w", err)
	}
	return loginRunner(ctx, codexPath, key, c.commandEnv())
}

func (c *processClient) ProviderInfo(ctx context.Context) (ProviderInfo, error) {
	path, err := exec.LookPath(codexBinary)
	if err != nil {
		return ProviderInfo{}, fmt.Errorf("agent/codexlive: find codex binary: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = c.commandEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ProviderInfo{}, fmt.Errorf("agent/codexlive: codex --version: %w: %s", err, firstNonEmptyLine(out))
	}
	version := firstNonEmptyLine(out)
	if version == "" {
		return ProviderInfo{}, errors.New("agent/codexlive: codex --version produced no output")
	}
	return ProviderInfo{Version: version, Binary: path}, nil
}

func (c *processClient) StartThread(ctx context.Context, req ThreadStartRequest) (ThreadInfo, error) {
	var resp threadStartResponse
	if err := c.call(ctx, "thread/start", map[string]any{
		"cwd":            req.CWD,
		"model":          nullIfEmpty(req.Model),
		"approvalPolicy": "on-request",
		"ephemeral":      false,
		"threadSource":   "user",
	}, &resp); err != nil {
		return ThreadInfo{}, err
	}
	return threadInfoFromResponse(resp.Thread, resp.Model), nil
}

func (c *processClient) ResumeThread(ctx context.Context, req ThreadResumeRequest) (ThreadInfo, error) {
	var resp threadStartResponse
	if err := c.call(ctx, "thread/resume", map[string]any{
		"threadId":       req.ThreadID,
		"cwd":            req.CWD,
		"model":          nullIfEmpty(req.Model),
		"approvalPolicy": "on-request",
	}, &resp); err != nil {
		return ThreadInfo{}, err
	}
	info := threadInfoFromResponse(resp.Thread, resp.Model)
	if info.ID == "" {
		info.ID = req.ThreadID
	}
	return info, nil
}

func (c *processClient) StartTurn(ctx context.Context, req TurnStartRequest) (TurnHandle, error) {
	params := map[string]any{
		"threadId":       req.ThreadID,
		"input":          []map[string]any{{"type": "text", "text": req.Prompt, "text_elements": []any{}}},
		"approvalPolicy": "on-request",
		"model":          nullIfEmpty(req.Model),
		"effort":         nullIfEmpty(req.ReasoningEffort),
	}
	if req.OutputSchema != nil {
		params["outputSchema"] = map[string]any(*req.OutputSchema)
	}
	var resp turnStartResponse
	if err := c.call(ctx, "turn/start", params, &resp); err != nil {
		return TurnHandle{}, err
	}
	if resp.Turn.ID == "" {
		return TurnHandle{}, errors.New("agent/codexlive: turn/start returned empty turn id")
	}
	return TurnHandle{TurnID: resp.Turn.ID, Events: c.registerTurnEvents(resp.Turn.ID)}, nil
}

func (c *processClient) RespondPermission(ctx context.Context, resp PermissionResponse) error {
	method := ""
	c.mu.Lock()
	if c.requestKinds != nil {
		method = c.requestKinds[resp.RequestID]
		delete(c.requestKinds, resp.RequestID)
	}
	c.mu.Unlock()
	if method == "" {
		method = serverRequestCommandApproval
	}
	result := permissionRPCResult(method, resp.Allow)
	return c.respond(ctx, resp.RequestID, result)
}

func (c *processClient) Close() error {
	c.mu.Lock()
	cmd := c.cmd
	stdin := c.stdin
	c.cmd = nil
	c.stdin = nil
	c.started = false
	c.closed = true
	var chans []chan ProviderEvent
	for _, ch := range c.turns {
		chans = append(chans, ch)
	}
	c.turns = nil
	c.earlyEvents = nil
	c.earlyClosed = nil
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	for _, ch := range chans {
		close(ch)
	}
	return nil
}

func (c *processClient) call(ctx context.Context, method string, params any, out any) error {
	if err := c.ensureStarted(ctx); err != nil {
		return err
	}
	id := c.nextRequestID()
	respCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[id] = respCh
	c.mu.Unlock()

	if err := c.writeJSON(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.dropPending(id)
		return err
	}
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil {
			return nil
		}
		if len(resp.Result) == 0 {
			return errors.New("agent/codexlive: empty JSON-RPC result")
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("agent/codexlive: decode %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.dropPending(id)
		return ctx.Err()
	}
}

func (c *processClient) respond(ctx context.Context, id string, result any) error {
	if id == "" {
		return errors.New("agent/codexlive: permission response missing request id")
	}
	return c.writeJSON(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *processClient) ensureStarted(ctx context.Context) error {
	c.mu.Lock()
	if c.started && c.cmd != nil {
		c.mu.Unlock()
		return nil
	}
	c.closed = false
	c.mu.Unlock()

	path, err := exec.LookPath(codexBinary)
	if err != nil {
		return fmt.Errorf("agent/codexlive: find codex binary: %w", err)
	}
	if err := c.ensureAPIKeyAuth(ctx, path); err != nil {
		return err
	}
	cmd := exec.Command(path, "app-server", "--stdio")
	cmd.Env = c.commandEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("agent/codexlive: app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("agent/codexlive: app-server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("agent/codexlive: app-server stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("agent/codexlive: start app-server: %w", err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.pending = map[string]chan rpcResponse{}
	c.turns = map[string]chan ProviderEvent{}
	c.earlyEvents = map[string][]ProviderEvent{}
	c.earlyClosed = map[string]bool{}
	c.requestKinds = map[string]string{}
	c.started = true
	c.mu.Unlock()

	go c.readStdout(stdout)
	go c.readStderr(stderr)

	var initResp initializeResponse
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "awf", "title": "AWF", "version": "0"},
		"capabilities": map[string]any{
			"experimentalApi":           true,
			"requestAttestation":        false,
			"optOutNotificationMethods": []any{},
		},
	}, &initResp); err != nil {
		_ = c.Close()
		return err
	}
	if err := c.writeJSON(ctx, map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

func (c *processClient) readStdout(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		c.handleLine(scanner.Bytes())
	}
	c.failPending(fmt.Errorf("agent/codexlive: app-server stdout closed: %s", c.stderrPreview()))
}

func (c *processClient) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 16*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		c.mu.Lock()
		c.lastStderr = line
		c.mu.Unlock()
	}
}

func (c *processClient) handleLine(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if len(msg.ID) > 0 && (len(msg.Result) > 0 || msg.Error != nil) {
		c.deliverResponse(msg)
		return
	}
	if len(msg.ID) > 0 && msg.Method != "" {
		c.handleServerRequest(msg)
		return
	}
	if msg.Method != "" {
		c.handleNotification(msg)
	}
}

func (c *processClient) deliverResponse(msg rpcMessage) {
	id := idKey(msg.ID)
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	// msg.Error is a *RPCError; assigning a nil pointer straight into the
	// error-typed field would yield a non-nil interface (typed-nil), so every
	// SUCCESSFUL response would be misread as an error whose Error() is "<nil>".
	// Convert explicitly so a success stays a true nil error.
	var rerr error
	if msg.Error != nil {
		rerr = msg.Error
	}
	ch <- rpcResponse{Result: msg.Result, Error: rerr}
}

func (c *processClient) handleServerRequest(msg rpcMessage) {
	id := idKey(msg.ID)
	ev, ok := permissionEventFromRequest(id, msg.Method, msg.Params)
	if !ok {
		_ = c.respond(context.Background(), id, map[string]any{"error": "unsupported request"})
		return
	}
	c.mu.Lock()
	c.ensureTurnEventMapsLocked()
	if c.requestKinds == nil {
		c.requestKinds = map[string]string{}
	}
	c.requestKinds[id] = msg.Method
	turnID := ""
	if ev.Permission != nil {
		turnID = ev.Permission.TurnID
	}
	ch := c.turns[turnID]
	if ch == nil && turnID != "" {
		c.earlyEvents[turnID] = append(c.earlyEvents[turnID], ev)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if ch == nil {
		_ = c.RespondPermission(context.Background(), PermissionResponse{RequestID: id, Allow: false})
		return
	}
	ch <- ev
}

func (c *processClient) handleNotification(msg rpcMessage) {
	ev, turnID, closeTurn, ok := providerEventFromNotification(msg.Method, msg.Params)
	if !ok {
		return
	}
	if turnID == "" {
		return
	}
	c.mu.Lock()
	c.ensureTurnEventMapsLocked()
	ch := c.turns[turnID]
	if closeTurn {
		delete(c.turns, turnID)
	}
	if ch == nil {
		c.earlyEvents[turnID] = append(c.earlyEvents[turnID], ev)
		if closeTurn {
			c.earlyClosed[turnID] = true
		}
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	ch <- ev
	if closeTurn {
		close(ch)
	}
}

func (c *processClient) registerTurnEvents(turnID string) <-chan ProviderEvent {
	c.mu.Lock()
	c.ensureTurnEventMapsLocked()
	pending := append([]ProviderEvent(nil), c.earlyEvents[turnID]...)
	closed := c.earlyClosed[turnID]
	delete(c.earlyEvents, turnID)
	delete(c.earlyClosed, turnID)
	capacity := 16
	if need := len(pending) + 1; need > capacity {
		capacity = need
	}
	events := make(chan ProviderEvent, capacity)
	if !closed {
		c.turns[turnID] = events
	}
	c.mu.Unlock()

	for _, ev := range pending {
		events <- ev
	}
	if closed {
		close(events)
	}
	return events
}

func (c *processClient) ensureTurnEventMapsLocked() {
	if c.turns == nil {
		c.turns = map[string]chan ProviderEvent{}
	}
	if c.earlyEvents == nil {
		c.earlyEvents = map[string][]ProviderEvent{}
	}
	if c.earlyClosed == nil {
		c.earlyClosed = map[string]bool{}
	}
}

func (c *processClient) writeJSON(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("agent/codexlive: app-server stdin closed")
	}
	done := make(chan error, 1)
	go func() {
		_, err := stdin.Write(data)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *processClient) nextRequestID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return "awf-" + strconv.FormatInt(c.nextID, 10)
}

func (c *processClient) dropPending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *processClient) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan rpcResponse{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcResponse{Error: err}
	}
}

func (c *processClient) stderrPreview() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastStderr
}

func (c *processClient) commandEnv() []string {
	env := os.Environ()
	for k, v := range c.env {
		env = upsertEnv(env, k, v)
	}
	return env
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage
	Error  error
}

type initializeResponse struct {
	UserAgent string `json:"userAgent"`
}

type threadStartResponse struct {
	Thread threadDTO `json:"thread"`
	// Model is the RESOLVED model the app-server picked for the thread (a required,
	// non-null string even when the request omitted model). It rides at the TOP
	// level of the thread/start (and thread/resume) response, NOT inside `thread`.
	Model string `json:"model"`
}

type threadDTO struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	SessionID string `json:"sessionId"`
}

type turnStartResponse struct {
	Turn turnDTO `json:"turn"`
}

type turnDTO struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Error  *turnErrorDTO `json:"error"`
}

type agentDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type itemCompletedNotification struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Item     json.RawMessage `json:"item"`
}

type turnCompletedNotification struct {
	ThreadID string  `json:"threadId"`
	Turn     turnDTO `json:"turn"`
}

// threadTokenUsageNotification is thread/tokenUsage/updated. tokenUsage.last is the
// just-completed turn's breakdown; tokenUsage.total is the cumulative thread total.
// AWF metrics are per-step (per-turn), so we read `last`. reasoningOutputTokens is a
// subset of outputTokens (matches agent/codex's documented mapping) so it is not
// added, to avoid double-counting output.
type threadTokenUsageNotification struct {
	ThreadID   string              `json:"threadId"`
	TurnID     string              `json:"turnId"`
	TokenUsage threadTokenUsageDTO `json:"tokenUsage"`
}

type threadTokenUsageDTO struct {
	Last  tokenUsageBreakdownDTO `json:"last"`
	Total tokenUsageBreakdownDTO `json:"total"`
}

type tokenUsageBreakdownDTO struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
	// TotalTokens is the runtime oracle for cache inclusion: whether cached is a
	// subset of input (total == input+output) or disjoint (total == input+cached+
	// output). normalizeForPricing reads it to decide if cached is subtracted.
	TotalTokens int `json:"totalTokens"`
}

type threadItemProbe struct {
	Type             string `json:"type"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregatedOutput"`
	ExitCode         *int   `json:"exitCode"`
}

type commandApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Command  string `json:"command"`
	CWD      string `json:"cwd"`
	Reason   string `json:"reason"`
}

type fileApprovalParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	GrantRoot string `json:"grantRoot"`
	Reason    string `json:"reason"`
}

type permissionsApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	CWD      string `json:"cwd"`
	Reason   string `json:"reason"`
}

type turnErrorDTO struct {
	Message           string `json:"message"`
	AdditionalDetails string `json:"additionalDetails"`
}

func providerEventFromNotification(method string, params json.RawMessage) (ProviderEvent, string, bool, bool) {
	switch method {
	case EventAgentMessageDelta:
		var p agentDeltaNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, "", false, false
		}
		return ProviderEvent{Type: EventAgentMessageDelta, Text: p.Delta}, p.TurnID, false, true
	case EventReasoningSummaryDelta:
		// The only liveness codex exposes while reasoning. Same delta shape as
		// agentMessage; forwarded non-terminal so the drain loop feeds the idle timer.
		var p agentDeltaNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, "", false, false
		}
		return ProviderEvent{Type: EventReasoningSummaryDelta, Text: p.Delta}, p.TurnID, false, true
	case EventItemCompleted:
		var p itemCompletedNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, "", false, false
		}
		var item threadItemProbe
		if err := json.Unmarshal(p.Item, &item); err != nil {
			return ProviderEvent{}, "", false, false
		}
		switch item.Type {
		case itemTypeAgentMessage:
			return ProviderEvent{Type: EventItemCompleted, ItemType: item.Type, Text: item.Text}, p.TurnID, false, true
		case itemTypeCommandExecution:
			// P2: surface the shell command + its output; Text carries the
			// aggregated output, Command the command line, ExitCode the status.
			// The app-server names this item "commandExecution" (camelCase) — verified
			// against a live codex app-server, NOT the codex CLI's "command_execution".
			return ProviderEvent{Type: EventItemCompleted, ItemType: item.Type, Command: item.Command, Text: item.AggregatedOutput, ExitCode: item.ExitCode}, p.TurnID, false, true
		default:
			// Other item types (reasoning, fileChange, …) stream via their own
			// events or aren't surfaced; dropping avoids duplicate/raw output.
			return ProviderEvent{}, "", false, false
		}
	case EventThreadTokenUsage:
		var p threadTokenUsageNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, "", false, false
		}
		return ProviderEvent{Type: EventThreadTokenUsage, Usage: Usage{
			InputTokens:       p.TokenUsage.Last.InputTokens,
			OutputTokens:      p.TokenUsage.Last.OutputTokens,
			CachedInputTokens: p.TokenUsage.Last.CachedInputTokens,
			TotalTokens:       p.TokenUsage.Last.TotalTokens,
		}}, p.TurnID, false, true
	case EventTurnCompleted:
		var p turnCompletedNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, "", false, false
		}
		return ProviderEvent{Type: EventTurnCompleted, Status: p.Turn.Status, Error: p.Turn.errorMessage()}, p.Turn.ID, true, true
	default:
		return ProviderEvent{}, "", false, false
	}
}

func (t turnDTO) errorMessage() string {
	if t.Error == nil {
		return ""
	}
	if t.Error.Message != "" {
		return t.Error.Message
	}
	return t.Error.AdditionalDetails
}

func permissionEventFromRequest(id, method string, params json.RawMessage) (ProviderEvent, bool) {
	switch method {
	case serverRequestCommandApproval:
		var p commandApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, false
		}
		return ProviderEvent{Type: EventPermissionRequest, Permission: &PermissionRequest{
			ID: id, Kind: "command", ToolID: "shell", Path: p.CWD, Command: p.Command, TurnID: p.TurnID,
		}}, true
	case serverRequestFileApproval:
		var p fileApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, false
		}
		return ProviderEvent{Type: EventPermissionRequest, Permission: &PermissionRequest{
			ID: id, Kind: "file", ToolID: "file_change", Path: p.GrantRoot, TurnID: p.TurnID,
		}}, true
	case serverRequestPermissionsApproval:
		var p permissionsApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return ProviderEvent{}, false
		}
		return ProviderEvent{Type: EventPermissionRequest, Permission: &PermissionRequest{
			ID: id, Kind: "permissions", ToolID: "permission_profile", Path: p.CWD, TurnID: p.TurnID,
		}}, true
	default:
		return ProviderEvent{}, false
	}
}

func permissionRPCResult(method string, allow bool) any {
	switch method {
	case serverRequestFileApproval:
		if allow {
			return map[string]any{"decision": "accept"}
		}
		return map[string]any{"decision": "decline"}
	case serverRequestPermissionsApproval:
		if allow {
			return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		}
		return map[string]any{"permissions": map[string]any{}, "scope": "turn", "strictAutoReview": true}
	default:
		if allow {
			return map[string]any{"decision": "accept"}
		}
		return map[string]any{"decision": "decline"}
	}
}

func threadInfoFromResponse(t threadDTO, model string) ThreadInfo {
	return ThreadInfo{ID: t.ID, TranscriptPath: t.Path, Model: model}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func idKey(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return string(bytes.TrimSpace(raw))
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func firstNonEmptyLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
