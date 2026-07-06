package ir

import "strings"

// validateStagingLiteral warns (AWF3015) when a run:/reduce.run command hardcodes
// the docker-only staging path "/work/.awf". $AWF_STAGING_ROOT is docker's fixed
// in-container staging root, but native's staging root is workdir-relative
// (".awf") — a literal "/work/.awf" is silently wrong there. Warning, not error:
// the literal is valid on docker, so this is a portability advisory, not a
// structural defect.
func validateStagingLiteral(ld *LoadedDefinition, c *collector) {
	if ld == nil || ld.Workflow == nil {
		return
	}
	WalkNodes(ld.Workflow.Graph, "", func(n Node, nodePath string) {
		switch v := n.(type) {
		case *CodeStep:
			if strings.Contains(v.Run, "/work/.awf") {
				c.warnf(nodePath+".run", "AWF3015", catalog["AWF3015"])
			}
		case *Map:
			if v.Reduce != nil && strings.Contains(v.Reduce.Run, "/work/.awf") {
				c.warnf(nodePath+".reduce.run", "AWF3015", catalog["AWF3015"])
			}
		}
	})
}
