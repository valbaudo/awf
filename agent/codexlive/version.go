package codexlive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type appServerSchemaShape struct {
	Methods  []string `json:"methods"`
	Events   []string `json:"events"`
	Requests []string `json:"requests"`
}

func AppServerSchemaDigest() string {
	shape := appServerSchemaShape{
		Methods:  []string{"thread/start", "thread/resume", "turn/start"},
		Events:   []string{EventAgentMessageDelta, EventItemCompleted, EventTurnCompleted},
		Requests: []string{EventPermissionRequest},
	}
	data, err := json.Marshal(shape)
	if err != nil {
		panic("agent/codexlive: marshal schema shape: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deterministicBackoff(n int, base time.Duration) []time.Duration {
	if n <= 0 || base <= 0 {
		return nil
	}
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = base << i
	}
	return out
}
