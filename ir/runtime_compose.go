package ir

// FirstRuntimeComposePath returns the first static path of a runtime compose
// block, or ("", false) when the workflow does not use runtime compose.
func FirstRuntimeComposePath(wf *Workflow) (string, bool) {
	if wf == nil {
		return "", false
	}
	var first string
	WalkNodes(wf.Graph, "", func(n Node, path string) {
		if first != "" {
			return
		}
		if _, ok := n.(*Compose); ok {
			first = path
		}
	})
	return first, first != ""
}
