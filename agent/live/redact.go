package live

import "regexp"

var (
	authHeaderRE       = regexp.MustCompile(`(?i)(\b(?:authorization|proxy-authorization)\s*:\s*)(?:bearer|basic)\s+[^\s]+`)
	secretKeyShape     = `(?i:"?(?:(?:[a-z0-9]+[_-])*(?:api[_-]?key|key|token|secret|password|passwd|authorization|transcript[_-]?path)(?:[_-][a-z0-9]+)*)"?\s*[:=]\s*)`
	secretQuotedPairRE = regexp.MustCompile(`(` + secretKeyShape + `)"[^"\r\n]*"`)
	secretSinglePairRE = regexp.MustCompile(`(` + secretKeyShape + `)'[^'\r\n]*'`)
	secretBarePairRE   = regexp.MustCompile(`(` + secretKeyShape + `)[^"'\s,}\r\n]+`)
)

func RedactKnownSecretShapes(s string) string {
	s = authHeaderRE.ReplaceAllString(s, "${1}[redacted]")
	s = secretQuotedPairRE.ReplaceAllString(s, `${1}"[redacted]"`)
	s = secretSinglePairRE.ReplaceAllString(s, `${1}'[redacted]'`)
	return secretBarePairRE.ReplaceAllString(s, "${1}[redacted]")
}
