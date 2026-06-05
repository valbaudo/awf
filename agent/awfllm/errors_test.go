package awfllm_test

import (
	"strings"
	"testing"

	"github.com/valbaudo/awf/agent/awfllm"
)

func TestErrMissingAPIKey_Message(t *testing.T) {
	e := &awfllm.ErrMissingAPIKey{RequiredKey: "OPENAI_API_KEY", AvailableKeys: []string{"FOO"}}
	if !strings.Contains(e.Error(), "OPENAI_API_KEY") {
		t.Errorf("Error() = %q", e.Error())
	}
}
