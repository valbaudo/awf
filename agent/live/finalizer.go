package live

func FinalizeCommittedTurn(root Root, adapterRef, sessionKey string, turn CommittedTurn) error {
	return withSessionLock(root, adapterRef, sessionKey, func() error {
		rec, err := ReadSessionRecord(root, adapterRef, sessionKey)
		if err != nil {
			return err
		}
		if rec.ActiveTurn != nil && !activeTurnMatchesCommit(*rec.ActiveTurn, turn) {
			return ErrActiveTurnMismatch
		}
		rec.ActiveTurn = nil
		rec.LastCommittedTurn = &turn
		return WriteSessionRecord(root, rec)
	})
}

func activeTurnMatchesCommit(active ActiveTurn, turn CommittedTurn) bool {
	if active.RunID != turn.RunID || active.NodePath != turn.NodePath || active.NextEpoch != turn.Epoch {
		return false
	}
	if active.ProviderTurnID != turn.ProviderTurnID {
		return false
	}
	return true
}
