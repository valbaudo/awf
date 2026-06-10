package live

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const LeaseSchema = "awf-live-lease-v1"

type LeaseRequest struct {
	AdapterRef    string `json:"adapter_ref"`
	SessionKey    string `json:"session_key"`
	OwnerRunID    string `json:"owner_run_id"`
	OwnerPID      int    `json:"owner_pid"`
	OwnerNodePath string `json:"owner_node_path"`
	OwnerEpoch    int    `json:"owner_epoch"`
	LeaseID       string `json:"lease_id"`
	TTLSeconds    int64  `json:"ttl_seconds"`
}

type LeaseRecord struct {
	Schema        string `json:"schema"`
	AdapterRef    string `json:"adapter_ref"`
	SessionKey    string `json:"session_key"`
	OwnerRunID    string `json:"owner_run_id"`
	OwnerPID      int    `json:"owner_pid"`
	OwnerNodePath string `json:"owner_node_path"`
	OwnerEpoch    int    `json:"owner_epoch"`
	LeaseID       string `json:"lease_id"`
	TTLSeconds    int64  `json:"ttl_seconds"`
	AcquiredUnix  int64  `json:"acquired_unix"`
	HeartbeatUnix int64  `json:"heartbeat_unix"`
}

func LeaseID(runID string, epoch int, nodePath, sessionKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s", runID, epoch, nodePath, sessionKey)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func AcquireLease(root Root, req LeaseRequest, now time.Time, runHeld func(string) bool) (LeaseRecord, error) {
	if err := ValidateSessionKey(req.SessionKey); err != nil {
		return LeaseRecord{}, err
	}
	if req.TTLSeconds <= 0 {
		return LeaseRecord{}, fmt.Errorf("agent/live: ttl_seconds must be positive")
	}
	var acquired LeaseRecord
	err := withSessionLock(root, req.AdapterRef, req.SessionKey, func() error {
		path := leasePath(root, req.AdapterRef, req.SessionKey)
		existing, found, err := readLease(path)
		if err != nil {
			return err
		}
		if found {
			if existing.LeaseID == req.LeaseID {
				next := existing
				next.Schema = LeaseSchema
				next.HeartbeatUnix = now.Unix()
				next.TTLSeconds = req.TTLSeconds
				if err := writeJSONAtomic(path, next); err != nil {
					return err
				}
				acquired = next
				return nil
			}
			if !leaseStale(existing, now) {
				return ErrLiveLeaseConflict
			}
			if runHeld == nil || runHeld(existing.OwnerRunID) {
				return ErrLiveLeaseStaleOwned
			}
		}
		next := LeaseRecord{
			Schema:        LeaseSchema,
			AdapterRef:    req.AdapterRef,
			SessionKey:    req.SessionKey,
			OwnerRunID:    req.OwnerRunID,
			OwnerPID:      req.OwnerPID,
			OwnerNodePath: req.OwnerNodePath,
			OwnerEpoch:    req.OwnerEpoch,
			LeaseID:       req.LeaseID,
			TTLSeconds:    req.TTLSeconds,
			AcquiredUnix:  now.Unix(),
			HeartbeatUnix: now.Unix(),
		}
		if err := writeJSONAtomic(path, next); err != nil {
			return err
		}
		acquired = next
		return nil
	})
	return acquired, err
}

func ReleaseLease(root Root, adapterRef, sessionKey, leaseID string) error {
	if err := ValidateSessionKey(sessionKey); err != nil {
		return err
	}
	return withSessionLock(root, adapterRef, sessionKey, func() error {
		path := leasePath(root, adapterRef, sessionKey)
		existing, found, err := readLease(path)
		if err != nil {
			return err
		}
		if !found || existing.LeaseID != leaseID {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return wrapIO("remove lease", err)
		}
		return syncDir(filepath.Dir(path))
	})
}

func leasePath(root Root, adapterRef, sessionKey string) string {
	return filepath.Join(root.Path, "leases", escapePathComponent(adapterRef), escapePathComponent(sessionKey)+".json")
}

func readLease(path string) (LeaseRecord, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LeaseRecord{}, false, nil
		}
		return LeaseRecord{}, false, wrapIO("read lease", err)
	}
	var rec LeaseRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return LeaseRecord{}, false, wrapIO("decode lease", err)
	}
	return rec, true, nil
}

func leaseStale(rec LeaseRecord, now time.Time) bool {
	if rec.TTLSeconds <= 0 {
		return true
	}
	return now.Unix() >= rec.HeartbeatUnix+rec.TTLSeconds
}
