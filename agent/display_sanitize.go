package agent

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	displaySKPattern         = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	displayAuthHeaderPattern = regexp.MustCompile(`(?i)(\b(?:authorization|proxy-authorization)\s*:\s*)(?:bearer|basic)\s+[^\s]+`)
	displaySecretKeyPattern  = `(?i:"?(?:(?:[a-z0-9]+[_-])*(?:api[_-]?key|key|token|secret|password|passwd|authorization)(?:[_-][a-z0-9]+)*)"?\s*[:=]\s*)`
	displaySecretQuotedPair  = regexp.MustCompile(`(` + displaySecretKeyPattern + `)"[^"\r\n]*"`)
	displaySecretSinglePair  = regexp.MustCompile(`(` + displaySecretKeyPattern + `)'[^'\r\n]*'`)
	displaySecretBarePair    = regexp.MustCompile(`(` + displaySecretKeyPattern + `)[^"'\s,}\r\n]+`)
)

// SanitizeDisplayText removes terminal control sequences and non-printing
// controls from text that may be rendered or copied into durable previews.
// Newline and tab are preserved because existing renderers use them for layout.
func SanitizeDisplayText(s string) string {
	return SanitizeDisplayBytes([]byte(s))
}

// RedactDisplayText removes common inline secret shapes from already-sanitized
// display text. It is best-effort defense-in-depth for previews, not a promise
// that provider-owned raw transcripts contain no secrets.
func RedactDisplayText(s string) string {
	s = displayAuthHeaderPattern.ReplaceAllString(s, "${1}[redacted]")
	s = displaySecretQuotedPair.ReplaceAllString(s, `${1}"[redacted]"`)
	s = displaySecretSinglePair.ReplaceAllString(s, `${1}'[redacted]'`)
	s = displaySecretBarePair.ReplaceAllString(s, "${1}[redacted]")
	return displaySKPattern.ReplaceAllString(s, "sk-[redacted]")
}

// SanitizeDisplayBytes is the byte-oriented variant for provider payloads. It
// drops invalid UTF-8 instead of replacing it, so previews cannot smuggle raw
// control bytes through replacement rendering.
func SanitizeDisplayBytes(in []byte) string {
	var b strings.Builder
	b.Grow(len(in))
	for i := 0; i < len(in); {
		c := in[i]
		switch {
		case c == 0x1b:
			i = skipEscapeSequence(in, i+1)
		case c == 0x9b:
			i = skipControlSequence(in, i+1)
		case isC1StringControlByte(c):
			i = skipStringControl(in, i+1)
		case c < 0x20 || c == 0x7f:
			if c == '\n' || c == '\t' {
				b.WriteByte(c)
			}
			i++
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
		default:
			r, size := utf8.DecodeRune(in[i:])
			if r == utf8.RuneError && size == 1 {
				i++
				continue
			}
			switch {
			case r == 0x9b:
				i = skipControlSequence(in, i+size)
				continue
			case isC1StringControlRune(r):
				i = skipStringControl(in, i+size)
				continue
			case !isDisplayControl(r):
				b.WriteRune(r)
			}
			i += size
		}
	}
	return b.String()
}

type displayStreamMode uint8

const (
	displayStreamNormal displayStreamMode = iota
	displayStreamEsc
	displayStreamCSI
	displayStreamString
	displayStreamStringEsc
)

// DisplayStreamSanitizer sanitizes a sequence of chunks from one terminal-like
// stream. It carries incomplete escape/control sequences across chunks so a
// provider cannot split OSC/DCS/APC/PM content and leak the tail in the next
// AgentEvent.
type DisplayStreamSanitizer struct {
	mode displayStreamMode
}

func (s *DisplayStreamSanitizer) SanitizeText(chunk string) string {
	return s.SanitizeBytes([]byte(chunk))
}

func (s *DisplayStreamSanitizer) SanitizeBytes(in []byte) string {
	var b strings.Builder
	b.Grow(len(in))
	for i := 0; i < len(in); {
		switch s.mode {
		case displayStreamEsc:
			next, complete, mode := scanEscContinuation(in, i)
			i = next
			if !complete {
				s.mode = mode
				return b.String()
			}
			s.mode = displayStreamNormal
			continue
		case displayStreamCSI:
			next, complete := scanControlSequence(in, i)
			i = next
			if !complete {
				return b.String()
			}
			s.mode = displayStreamNormal
			continue
		case displayStreamString:
			next, complete, mode := scanStringControl(in, i)
			i = next
			if !complete {
				s.mode = mode
				return b.String()
			}
			s.mode = displayStreamNormal
			continue
		case displayStreamStringEsc:
			if i >= len(in) {
				return b.String()
			}
			if in[i] == '\\' {
				i++
				s.mode = displayStreamNormal
				continue
			}
			s.mode = displayStreamString
			continue
		}

		c := in[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(in) {
				s.mode = displayStreamEsc
				return b.String()
			}
			next, complete, mode := scanEscContinuation(in, i+1)
			i = next
			if !complete {
				s.mode = mode
				return b.String()
			}
		case c == 0x9b:
			next, complete := scanControlSequence(in, i+1)
			i = next
			if !complete {
				s.mode = displayStreamCSI
				return b.String()
			}
		case isC1StringControlByte(c):
			next, complete, mode := scanStringControl(in, i+1)
			i = next
			if !complete {
				s.mode = mode
				return b.String()
			}
		case c < 0x20 || c == 0x7f:
			if c == '\n' || c == '\t' {
				b.WriteByte(c)
			}
			i++
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
		default:
			if !utf8.FullRune(in[i:]) {
				return b.String()
			}
			r, size := utf8.DecodeRune(in[i:])
			if r == utf8.RuneError && size == 1 {
				i++
				continue
			}
			switch {
			case r == 0x9b:
				next, complete := scanControlSequence(in, i+size)
				i = next
				if !complete {
					s.mode = displayStreamCSI
					return b.String()
				}
				continue
			case isC1StringControlRune(r):
				next, complete, mode := scanStringControl(in, i+size)
				i = next
				if !complete {
					s.mode = mode
					return b.String()
				}
				continue
			case !isDisplayControl(r):
				b.WriteRune(r)
			}
			i += size
		}
	}
	return b.String()
}

func isDisplayControl(r rune) bool {
	return unicode.IsControl(r) && r != '\n' && r != '\t'
}

func isC1StringControlByte(c byte) bool {
	switch c {
	case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
		return true
	default:
		return false
	}
}

func isC1StringControlRune(r rune) bool {
	switch r {
	case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
		return true
	default:
		return false
	}
}

func skipEscapeSequence(in []byte, i int) int {
	if i >= len(in) {
		return i
	}
	switch in[i] {
	case '[':
		return skipControlSequence(in, i+1)
	case ']', 'P', '^', '_', 'X':
		return skipStringControl(in, i+1)
	default:
		return i + 1
	}
}

func skipControlSequence(in []byte, i int) int {
	for i < len(in) {
		c := in[i]
		i++
		if c >= 0x40 && c <= 0x7e {
			break
		}
	}
	return i
}

func scanEscContinuation(in []byte, i int) (int, bool, displayStreamMode) {
	if i >= len(in) {
		return i, false, displayStreamEsc
	}
	switch in[i] {
	case '[':
		next, complete := scanControlSequence(in, i+1)
		return next, complete, displayStreamCSI
	case ']', 'P', '^', '_', 'X':
		next, complete, mode := scanStringControl(in, i+1)
		return next, complete, mode
	default:
		return i + 1, true, displayStreamNormal
	}
}

func scanControlSequence(in []byte, i int) (int, bool) {
	for i < len(in) {
		c := in[i]
		i++
		if c >= 0x40 && c <= 0x7e {
			return i, true
		}
	}
	return i, false
}

func scanStringControl(in []byte, i int) (int, bool, displayStreamMode) {
	for i < len(in) {
		switch in[i] {
		case 0x07, 0x9c:
			return i + 1, true, displayStreamNormal
		case 0x1b:
			if i+1 < len(in) && in[i+1] == '\\' {
				return i + 2, true, displayStreamNormal
			}
			if i+1 >= len(in) {
				return i + 1, false, displayStreamStringEsc
			}
		case 0xc2:
			if i+1 < len(in) && in[i+1] == 0x9c {
				return i + 2, true, displayStreamNormal
			}
		}
		i++
	}
	return i, false, displayStreamString
}

func skipStringControl(in []byte, i int) int {
	for i < len(in) {
		switch in[i] {
		case 0x07, 0x9c:
			return i + 1
		case 0x1b:
			if i+1 < len(in) && in[i+1] == '\\' {
				return i + 2
			}
		case 0xc2:
			if i+1 < len(in) && in[i+1] == 0x9c {
				return i + 2
			}
		}
		i++
	}
	return i
}
