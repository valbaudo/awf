package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/valbaudo/awf/agent/live"
	"github.com/valbaudo/awf/engine"
)

func openLiveHomeRoot(stateDir string) (live.Root, error) {
	return live.OpenRoot(stateDir, liveHomeEnv())
}

func checkLiveHomePin(pin *engine.LiveHomePin, stateDir string) error {
	if pin == nil {
		return nil
	}
	return live.CheckHomePin(live.HomePin{Path: pin.Path, Digest: pin.Digest}, stateDir, liveHomeEnv())
}

func engineLiveHomePin(pin live.HomePin) *engine.LiveHomePin {
	return &engine.LiveHomePin{Path: pin.Path, Digest: pin.Digest}
}

func liveDispatchFinalizer(root live.Root) func(context.Context, engine.LiveDispatchRecord) error {
	return func(ctx context.Context, rec engine.LiveDispatchRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rec.AdapterRef == "" || rec.SessionKey == "" {
			return fmt.Errorf("cli: live finalizer missing adapter/session metadata")
		}
		if err := live.FinalizeCommittedTurn(root, rec.AdapterRef, rec.SessionKey, live.CommittedTurn{
			RunID:          rec.RunID,
			NodePath:       rec.NodePath,
			Epoch:          int(rec.Epoch),
			ProviderTurnID: rec.ProviderTurnID,
			CommittedUnix:  rec.CommittedUnix,
		}); err != nil {
			return err
		}
		if rec.LeaseID != "" {
			if err := live.ReleaseLease(root, rec.AdapterRef, rec.SessionKey, rec.LeaseID); err != nil {
				return err
			}
		}
		return nil
	}
}

func liveHomeEnv() map[string]string {
	value, ok := os.LookupEnv("AWF_LIVE_HOME")
	if !ok {
		return nil
	}
	return map[string]string{"AWF_LIVE_HOME": value}
}
