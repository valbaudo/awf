package obs

import (
	"fmt"
	"sort"
	"time"

	"github.com/valbaudo/awf/engine"
)

func attachSkillsSelectedEvent(s *Span, d engine.SkillsSelectedData, ts time.Time) {
	attrs := map[string]any{
		AttrSkillsLibrary:       d.Library,
		AttrSkillsLibraryDigest: d.LibraryDigest,
		AttrSkillsRouter:        d.Router,
		AttrSkillsRouterVersion: d.RouterVersion,
		AttrSkillsSelectedCount: int64(len(d.Selected)),
	}
	for i, selected := range d.Selected {
		prefix := fmt.Sprintf("%s%d.", AttrSkillsSelectedPre, i)
		attrs[prefix+"id"] = selected.ID
		attrs[prefix+"score"] = selected.Score
	}
	paramKeys := make([]string, 0, len(d.RouterParams))
	for k := range d.RouterParams {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)
	for _, k := range paramKeys {
		attrs[AttrSkillsRouterParamPre+k] = d.RouterParams[k]
	}
	s.Events = append(s.Events, SpanEvent{
		Name:       EventNameSkillsSelected,
		Time:       ts,
		Attributes: attrs,
	})
}
