package agent

import (
	"encoding/json"
	"fmt"
)

// PrependFeedback returns prompt with the gate's prior verdict prepended in the
// canonical "<previous verdict>\n<json>\n\n<prompt>" form, or prompt unchanged
// when feedback is empty. This is the single source of the repair-feedback wire
// format; every adapter that supports gate repair calls it. The format string
// and json.Marshal match the pre-refactor inline blocks byte-for-byte.
func PrependFeedback(prompt string, feedback map[string]any) (string, error) {
	if len(feedback) == 0 {
		return prompt, nil
	}
	fb, err := json.Marshal(feedback)
	if err != nil {
		return "", fmt.Errorf("agent: marshal gate feedback: %w", err)
	}
	return fmt.Sprintf("<previous verdict>\n%s\n\n%s", string(fb), prompt), nil
}
