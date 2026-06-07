package signal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// This file is the pause/cancel CONTROL surface of the Broker: the pause.json /
// cancel.json request bodies, their writers, and the engine-side check/clear. It
// shares the *Broker receiver and controlDir with the named-signal queue
// (broker.go) but is an independent cohesion unit — split out so neither half
// grows the other. Mirrors the package's existing one-file-per-concern layout.

// ErrPaused is the sentinel engine.Run returns alongside Outcome("") when
// control-file polling detects pause.json. The CLI's runAndFinish maps this
// to a clean exit (rc=0) WITHOUT writing run.finished — the run is non-
// terminal and resumable.
var ErrPaused = errors.New("signal: run paused (non-terminal)")

// ErrCancelled is the sentinel engine.Run returns alongside Outcome("") when
// control-file polling detects cancel.json. The engine has ALREADY appended
// the terminal run.cancelled event; the CLI exits cleanly. `awf resume`
// refuses any log with a run.cancelled event.
var ErrCancelled = errors.New("signal: run cancelled (terminal)")

// PauseRequest is the parsed body of pause.json.
type PauseRequest struct {
	NodePath string `json:"node_path,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// CancelRequest is the parsed body of cancel.json.
type CancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// WritePause writes pause.json. Idempotent — overwrites any existing file.
func (b *Broker) WritePause(req PauseRequest) error {
	return b.writeControlJSON(pauseFileName, "pause", req)
}

// WriteCancel writes cancel.json. Idempotent.
func (b *Broker) WriteCancel(req CancelRequest) error {
	return b.writeControlJSON(cancelFileName, "cancel", req)
}

// writeControlJSON serializes req as JSON and writes it to controlDir/filename
// after ensuring controlDir exists. The label appears in error messages to
// distinguish the call site (pause vs cancel). Shared by WritePause/WriteCancel.
func (b *Broker) writeControlJSON(filename, label string, req any) error {
	if err := os.MkdirAll(b.controlDir, 0o755); err != nil {
		return fmt.Errorf("signal: mkdir %q: %w", b.controlDir, err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("signal: marshal %s: %w", label, err)
	}
	path := filepath.Join(b.controlDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("signal: write %q: %w", path, err)
	}
	return nil
}

// maxControlFileBytes caps how much we read from pause.json / cancel.json.
// L10 fix: defense against an adversarial / misconfigured writer that
// redirects a huge stream into the control file (e.g. `head -c 1G /dev/zero
// > control/cancel.json`). AWF is an offensive security tool; adversarial
// inputs are part of the threat model. 64KiB is more than enough for a
// reasonable JSON reason string.
const maxControlFileBytes = 64 * 1024

// readControlFile reads up to maxControlFileBytes from path. Returns nil
// (no err) if the file doesn't exist. Errors only on I/O failures or
// genuinely-unexpected conditions.
func readControlFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxControlFileBytes))
}

// CheckPauseCancel reports whether pause.json and cancel.json exist in
// controlDir. Either bool may be true; both may be true (cancel-wins resolution
// is the caller's responsibility — engine/controls.go does that).
//
// Returns (pauseReq, cancelReq, err). pauseReq/cancelReq are non-nil iff their
// respective files exist. err is non-nil only on read errors (file exists but
// not readable; malformed JSON is silently treated as an empty body). Reads
// are capped at maxControlFileBytes (L10 fix).
func (b *Broker) CheckPauseCancel() (*PauseRequest, *CancelRequest, error) {
	pausePath := filepath.Join(b.controlDir, pauseFileName)
	cancelPath := filepath.Join(b.controlDir, cancelFileName)

	var pauseReq *PauseRequest
	if data, err := readControlFile(pausePath); err != nil {
		return nil, nil, fmt.Errorf("signal: read pause %q: %w", pausePath, err)
	} else if data != nil {
		var req PauseRequest
		_ = json.Unmarshal(data, &req) // empty/malformed treated as empty body
		pauseReq = &req
	}

	var cancelReq *CancelRequest
	if data, err := readControlFile(cancelPath); err != nil {
		return nil, nil, fmt.Errorf("signal: read cancel %q: %w", cancelPath, err)
	} else if data != nil {
		var req CancelRequest
		_ = json.Unmarshal(data, &req)
		cancelReq = &req
	}
	return pauseReq, cancelReq, nil
}

// ClearPauseCancel removes pause.json and cancel.json. Idempotent (missing
// files are not errors). Called by cli/resume.go before re-entering the
// engine — pause is non-terminal but stale pause.json would re-pause on the
// next commit; resume must clear it to make forward progress.
func (b *Broker) ClearPauseCancel() error {
	for _, name := range []string{pauseFileName, cancelFileName} {
		path := filepath.Join(b.controlDir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("signal: clear %q: %w", path, err)
		}
	}
	return nil
}
