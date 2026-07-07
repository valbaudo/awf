package ir

import "fmt"

// validateRecovery (AWF1064) rejects a retry.recovery value that is not one of
// the supported strategies {"continue", "restart", ""}. recovery selects how a
// retry re-runs a step after a transient fault (engine.effectiveRecovery); a
// typo like "contineu" would otherwise be silently treated as the per-adapter
// default ("restart" for a stateless adapter), so the author's intent to
// "continue" is lost with no signal. This mirrors the strict-value posture of
// AWF1062 (unknown key) and AWF1063 (bare-int duration): a fat-fingered enum is
// an error at load time, not a silent fallback.
//
// Unlike validateDurationScalars — which must inspect RawDoc to catch a bare-int
// misparse — recovery is a plain string faithfully carried into the typed IR
// (ir.RetryPolicy.Recovery), so this walks the typed graph. The field lives on
// every RetryPolicy occurrence (code steps, agent steps, and tool impls), so all
// three positions are checked, matching validateDurationScalars' coverage.
func validateRecovery(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	WalkNodes(ld.Workflow.Graph, "", func(n Node, nodePath string) {
		switch v := n.(type) {
		case *CodeStep:
			checkRecovery(v.Retry, nodePath+".retry", c)
		case *AgentStep:
			checkRecovery(v.Retry, nodePath+".retry", c)
		}
	})
	// tools: is a top-level map; a tool's retry lives under its impl (ir.ToolImpl).
	for name, tool := range ld.Workflow.Tools {
		checkRecovery(tool.Impl.Retry, "tools."+name+".impl.retry", c)
	}
}

// checkRecovery emits AWF1064 when rp carries a recovery value outside the
// supported set. A nil policy or an unset (empty) recovery is fine — the engine
// resolves the per-adapter default at dispatch time.
func checkRecovery(rp *RetryPolicy, path string, c *collector) {
	if rp == nil {
		return
	}
	switch rp.Recovery {
	case "", "continue", "restart":
		return
	default:
		c.errf(path, "AWF1064", fmt.Sprintf(
			"recovery: %q is not a supported strategy (want \"continue\", \"restart\", or unset)", rp.Recovery))
	}
}
