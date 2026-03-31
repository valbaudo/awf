// Package clock provides the Clock and IDGen interfaces, injected wherever time/ids are needed.
package clock

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Clock supplies the current time. Injected so tests can control it.
type Clock interface {
	Now() time.Time
}

// IDGen mints the run id — the only nondeterministic id in the runtime.
type IDGen interface {
	NewRunID() string
}

// System is the production Clock (UTC wall-clock).
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// runIDBytes is the byte length of a freshly minted run id (128 bits).
// runIDHexLen is the resulting hex-encoded string length, exported (lower-case, package-internal)
// for the test that asserts production IDs are the right shape.
const (
	runIDBytes  = 16
	runIDHexLen = 2 * runIDBytes
)

// CryptoIDGen is the production IDGen (128-bit crypto/rand, hex-encoded).
type CryptoIDGen struct{}

func (CryptoIDGen) NewRunID() string {
	var b [runIDBytes]byte
	// On Go 1.20+ crypto/rand.Read does not return errors — the stdlib crashes the process if the
	// OS RNG is unavailable. The branch below is retained defensively (and satisfies errcheck); if
	// a future runtime ever surfaces an error here, the panic gives a clear, attributed message
	// instead of silently corrupting a run.
	if _, err := rand.Read(b[:]); err != nil {
		panic("clock: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Fake is a deterministic Clock+IDGen for tests. NewRunID returns IDs in order.
type Fake struct {
	T   time.Time
	IDs []string
	i   int
}

func (f *Fake) Now() time.Time { return f.T }

func (f *Fake) NewRunID() string {
	if f.i >= len(f.IDs) {
		panic(fmt.Sprintf("clock.Fake: ran out of seeded IDs (seeded %d, call %d)", len(f.IDs), f.i+1))
	}
	id := f.IDs[f.i]
	f.i++
	return id
}
