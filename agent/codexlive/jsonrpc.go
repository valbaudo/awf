package codexlive

import (
	"context"
	"fmt"

	"github.com/valbaudo/awf/ir"
)

const (
	EventAgentMessageDelta = "item/agentMessage/delta"
	EventItemCompleted     = "item/completed"
	EventTurnCompleted     = "turn/completed"
	EventThreadTokenUsage  = "thread/tokenUsage/updated"
	EventPermissionRequest = "server/request/permission"
)

type Client interface {
	ProviderInfo(context.Context) (ProviderInfo, error)
	StartThread(context.Context, ThreadStartRequest) (ThreadInfo, error)
	ResumeThread(context.Context, ThreadResumeRequest) (ThreadInfo, error)
	StartTurn(context.Context, TurnStartRequest) (TurnHandle, error)
	RespondPermission(context.Context, PermissionResponse) error
}

type ProviderInfo struct {
	Version string
	Binary  string
}

type ThreadStartRequest struct {
	CWD             string
	Model           string
	ReasoningEffort string
}

type ThreadResumeRequest struct {
	ThreadID        string
	CWD             string
	Model           string
	ReasoningEffort string
}

type ThreadInfo struct {
	ID             string
	TmuxSession    string
	TranscriptPath string
}

type TurnStartRequest struct {
	ThreadID        string
	Prompt          string
	OutputSchema    *ir.JSONSchema
	Model           string
	ReasoningEffort string
}

type TurnHandle struct {
	TurnID string
	Events <-chan ProviderEvent
}

type ProviderEvent struct {
	Type       string
	Text       string
	Output     map[string]any
	Usage      Usage
	Status     string
	Error      string
	Permission *PermissionRequest
}

type Usage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
}

type PermissionRequest struct {
	ID      string
	TurnID  string
	Kind    string
	ToolID  string
	Path    string
	Command string
}

type PermissionResponse struct {
	RequestID string
	ThreadID  string
	TurnID    string
	Allow     bool
	Reason    string
}

type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

func isBackpressure(err error) bool {
	rpc, ok := err.(*RPCError)
	return ok && rpc.Code == -32001
}
