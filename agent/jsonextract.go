package agent

import "strings"

// StripThinkTags removes a leading reasoning block delimited by a closing think
// tag (</think> or </thinking>), keeping only the text AFTER the LAST such tag —
// reasoning models emit "...reasoning...</think>{json}". Uses the LAST tag (not
// the first) so multiple reasoning blocks are all dropped. If no closing tag is
// present, returns s unchanged (the brace-scan handles tag-less output).
func StripThinkTags(s string) string {
	for _, tag := range []string{"</thinking>", "</think>"} {
		if i := strings.LastIndex(s, tag); i >= 0 {
			return s[i+len(tag):]
		}
	}
	return s
}
