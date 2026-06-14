package awfllm

import (
	"context"
	"fmt"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/ir"
)

// callAnthropic is the native Anthropic Messages-API transport (POST /v1/messages,
// SSE streaming). Stub until Task 8.
func (a *Adapter) callAnthropic(ctx context.Context, cfg reqConfig, prompt string, schema *ir.JSONSchema, thread []agent.ThreadTurn, files []agent.InputFile, emit func(delta string, raw []byte)) (string, usageRec, string, string, error) {
	return "", usageRec{}, "", "", fmt.Errorf("agent/awfllm: callAnthropic not implemented")
}
