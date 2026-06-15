package ir

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/valbaudo/awf/template"
)

// maxExpressionBytes caps the size of an ir.Expr or ir.Template field before the validator
// will attempt to parse it. Adversarial workflows could otherwise submit multi-megabyte
// conditions to OOM the parser; the §7 mini-language has no legitimate need for anything
// longer than a few hundred bytes. 64 KiB is generous.
const maxExpressionBytes = 64 * 1024

// validateStructural runs the AWF1xxx pass: workflow-version, step-id uniqueness, container
// shape, image-is-digest, container-ref resolution (missing or unresolved), parallel/map
// distinct-container rule, loop/map/gate field requirements, expression-size limits, and the
// "no template syntax in static-name fields" rule (AWF1019). All diagnostics produced here
// are Error severity (the only Warnings in slice 1.4 are AWF2002 and AWF3002).
func validateStructural(ld *LoadedDefinition, c *collector) {
	wf := ld.Workflow

	// (P6a) Containers named by a map's `image:` receive their image per-element
	// at runtime; such a container may declare resources alone (no static
	// image/compose), so it is exempt from the "exactly one of image/compose"
	// requirement below. The exemption is path-aware: the target may only be
	// referenced by the owning map and steps inside that map's body.
	mapImageTargetOwners := MapImageTargetOwners(wf)
	mapImageTargets := mapImageTargetsFromOwners(mapImageTargetOwners)

	// (a) Workflow-level: only version 1 is defined (AWF §2 "Current: 1").
	if wf.Version != 1 {
		c.errf("", "AWF1017", fmt.Sprintf("%s (got %d)", catalog["AWF1017"], wf.Version))
	}

	// (b) Container shape: each must declare exactly one of image / compose; image must be a
	// digest reference; compose-backed containers must declare a service; neither image nor
	// service field may carry template syntax (those are static names).
	for name, ctr := range wf.Containers {
		if !containerNamePattern.MatchString(name) {
			c.errf(ContainerPath(name, ""), "AWF1059", fmt.Sprintf("%s: %q (must match %s)", catalog["AWF1059"], name, containerNamePattern))
		}
		switch {
		case ctr.Image != "" && ctr.Compose != "":
			c.errf(ContainerPath(name, ""), "AWF1005", catalog["AWF1005"])
		case ctr.Image == "" && ctr.Compose == "":
			if !mapImageTargets[name] {
				c.errf(ContainerPath(name, ""), "AWF1006", catalog["AWF1006"])
			}
		}
		if ctr.Image != "" && !strings.Contains(ctr.Image, "@sha256:") {
			c.errf(ContainerPath(name, "image"), "AWF1007", catalog["AWF1007"])
		}
		if ctr.Image != "" && strings.Contains(ctr.Image, "{{") {
			c.errf(ContainerPath(name, "image"), "AWF1019", catalog["AWF1019"])
		}
		if ctr.Compose != "" && ctr.Service == "" {
			c.errf(ContainerPath(name, ""), "AWF1008", catalog["AWF1008"])
		}
		if ctr.Service != "" && strings.Contains(ctr.Service, "{{") {
			c.errf(ContainerPath(name, "service"), "AWF1019", catalog["AWF1019"])
		}
		if ctr.Snapshot != "" && ctr.Snapshot != "workspace" {
			c.errf(ContainerPath(name, "snapshot"), "AWF1021", catalog["AWF1021"])
		}
		if ctr.Snapshot == "workspace" && ctr.Compose != "" {
			c.errf(ContainerPath(name, "snapshot"), "AWF1022", catalog["AWF1022"])
		}
		// AWF1025 (P6a): a map.image target supplies the per-element image at
		// dispatch, which unconditionally overwrites any static image/compose on
		// this container — so a static pin here is silently discarded. Fire ONLY
		// when both are present (a resources-only target is the intended shape and
		// is exempt from AWF1006 above). ConflictsWith semantics.
		if mapImageTargets[name] && (ctr.Image != "" || ctr.Compose != "") {
			c.errf(ContainerPath(name, ""), "AWF1025", catalog["AWF1025"])
		}
	}

	// (c) Workflow-level env: a list of host env-var NAMES forwarded into agent steps
	// (see awf-workflow(5) TOP LEVEL). Each entry must be a valid environment-variable
	// identifier — an empty or malformed name cannot name a real host var and would
	// silently drop from (or malform) the forwarded set. Values are never part of the
	// definition; only the names here fold into the digest.
	for i, name := range wf.Env {
		if !envNamePattern.MatchString(name) {
			c.errf(fmt.Sprintf("env[%d]", i), "AWF1024", fmt.Sprintf("%s: %q (must match %s)", catalog["AWF1024"], name, envNamePattern))
		}
	}

	// (d) Top-level assets use the same identifier charset and reserved-token
	// restrictions as step ids because `asset.<id>` references share the static
	// reference namespace shape with `step.<id>`.
	for id := range wf.Assets {
		checkAssetID(id, c)
	}

	// (d) Walk the graph: addressable-id uniqueness, container-ref resolution (missing OR unresolved),
	// control-node shape, parallel distinct-container rule, expression-size limits, AWF1019.
	seen := map[string]string{} // step/map-product id → first path where seen, for the duplicate diag
	walkStructural(wf.Graph, "", wf, c, seen, nil, mapImageTargetOwners)
}

// envNamePattern is the POSIX-portable environment-variable identifier charset. A workflow's
// top-level env: lists NAMES (never values); a name outside this charset can't address a real
// host variable, so the validator rejects it (AWF1024) rather than silently dropping it.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// containerNamePattern constrains container map keys to a path-safe identifier
// charset (mirrors stepIDPattern; bars '/', '\', '..', ':', '.'). The native
// backend derives a host workdir from this name (filepath.Join + RemoveAll on
// resume), so an unconstrained key is a path-traversal sink; docker sanitizes
// via containerName() but native does not. The validator sees RAW per-workflow
// keys ('::' qualification is composed at engine runtime), so the charset is strict.
var containerNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// NOTE: not built on ir.WalkNodes — walkStructural attaches diagnostics to nearly
// every control kind (AWF1010-1015) and does sibling-set / last-element checks
// (checkParallelDistinctContainers, AWF1014 on the final evaluate node). A visitor
// would save recursion calls but not the per-kind dispatch, and would put the
// load-bearing diagnostic path anchors one refactor-slip from drift. Keep bespoke.
//
// walkStructural recurses into nodes, computing each child's path via PathFor. parent is the
// path of the enclosing node (empty at the top level). wf is read-only — needed for container
// ref resolution (the set of declared container names).
//
// requireContainer is true for CodeStep / AgentStep (where AWF §4 requires a container) and
// false for SignalStep (where AWF §4.3 explicitly states "No container needed").
func walkStructural(nodes NodeList, parent string, wf *Workflow, c *collector, seen map[string]string, scoped map[string]bool, mapImageTargetOwners map[string][]string) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *CodeStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
			checkContainerRefInScope(v.Container, path, wf, scoped, mapImageTargetOwners, c, true /* required */)
			checkFieldSize(v.Run, path, c)
		case *AgentStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
			checkContainerRefInScope(v.Container, path, wf, scoped, mapImageTargetOwners, c, false /* optional: containerless adapters (awf/llm) need no container; run-start guard enforces it */)
		case *SignalStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
			// Slice 3.5 (M16): validate Await name charset to prevent path-traversal
			// characters, whitespace, nullbytes etc. from reaching the broker /
			// downstream consumers (OTel attributes, log scanners). Reuses
			// stepIDPattern — same charset semantics as step IDs.
			if v.Await != "" && !stepIDPattern.MatchString(v.Await) {
				c.errf(path, "AWF1020", fmt.Sprintf("%s: await=%q (must match %s)",
					catalog["AWF1020"], v.Await, stepIDPattern))
			}
			// SP4 keyed signals: the optional where: clause must be a bounded
			// boolean expression once its {{ }} slots are scanned/stripped (AWF1036).
			checkWhereExpr(v.Where, path+".where", c)
			// SignalStep has no container — by design (AWF §4.3).
		case *CallStep:
			path := PathFor(parent, "", v.ID, i)
			checkStepID(v.ID, path, c, seen)
		case *If:
			path := PathFor(parent, "if", "", i)
			checkFieldSize(string(v.Cond), path, c)
			walkStructural(v.Then, ChildPath(parent, "if", i, "then"), wf, c, seen, scoped, mapImageTargetOwners)
			walkStructural(v.Else, ChildPath(parent, "if", i, "else"), wf, c, seen, scoped, mapImageTargetOwners)
		case *Loop:
			path := PathFor(parent, "loop", "", i)
			if v.Until == nil && v.MaxIters == nil {
				c.errf(path, "AWF1011", catalog["AWF1011"])
			}
			if v.Until != nil {
				checkFieldSize(string(*v.Until), path, c)
			}
			walkStructural(v.Body, ChildPath(parent, "loop", i, "body"), wf, c, seen, scoped, mapImageTargetOwners)
		case *Try:
			walkStructural(v.Do, ChildPath(parent, "try", i, "do"), wf, c, seen, scoped, mapImageTargetOwners)
			walkStructural(v.Catch, ChildPath(parent, "try", i, "catch"), wf, c, seen, scoped, mapImageTargetOwners)
			walkStructural(v.Finally, ChildPath(parent, "try", i, "finally"), wf, c, seen, scoped, mapImageTargetOwners)
		case *Parallel:
			path := PathFor(parent, "parallel", "", i)
			checkParallelDistinctContainers(v.Children, path, c)
			walkStructural(v.Children, path, wf, c, seen, scoped, mapImageTargetOwners)
		case *Gate:
			path := PathFor(parent, "gate", "", i)
			if len(v.Generate) == 0 {
				c.errf(path, "AWF1013", catalog["AWF1013"])
			}
			if v.Until == "" {
				c.errf(path, "AWF1015", catalog["AWF1015"])
			} else {
				checkFieldSize(string(v.Until), path, c)
			}
			if len(v.Evaluate) > 0 {
				final := v.Evaluate[len(v.Evaluate)-1]
				if !nodeHasOutputSchema(final) {
					c.errf(path, "AWF1014", catalog["AWF1014"])
				}
			}
			walkStructural(v.Generate, ChildPath(parent, "gate", i, "generate"), wf, c, seen, scoped, mapImageTargetOwners)
			walkStructural(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), wf, c, seen, scoped, mapImageTargetOwners)
		case *Skip:
			// skip has no fields that need structural validation.
		case *React:
			// react[i] is the runtime path; v.ID is the output-addressing handle
			// ({{ <id>.* }}), sharing the addressable-id namespace with steps and
			// map aggregates — so its charset/reserved-token/uniqueness rules are
			// the same. No NodeList body to recurse. (tools/with/max_turns shape is
			// validated by validateTools, AWF1052–AWF1058.)
			path := PathFor(parent, "react", "", i)
			checkAddressableID(v.ID, path, "react node", c, seen)
		case *Map:
			path := PathFor(parent, "map", "", i)
			checkMapID(v, path, c, seen)
			if string(v.Over) == "" || v.As == "" || v.Container == "" || v.Concurrency == 0 {
				c.errf(path, "AWF1012", catalog["AWF1012"])
			}
			if v.Over != "" {
				checkFieldSize(string(v.Over), path, c)
			}
			if v.Image != "" {
				checkFieldSize(string(v.Image), path, c)
			}
			// Map.Container is a static container name (AWF §5.7) — must resolve, no `{{ }}`.
			if v.Container != "" {
				if strings.Contains(v.Container, "{{") {
					c.errf(path, "AWF1019", catalog["AWF1019"])
				} else {
					checkContainerRefInScope(v.Container, path, wf, scoped, mapImageTargetOwners, c, true /* required */)
				}
			}
			// AWF1023: snapshot:workspace on the map's fanned-out container would collide
			// bare-name snapshot keying across per-item instances. Temporary guard until a
			// later slice designs path-keyed per-item snapshots.
			if v.Container != "" && !strings.Contains(v.Container, "{{") {
				bare := v.Container
				if j := strings.Index(v.Container, ":"); j >= 0 {
					bare = v.Container[:j]
				}
				if ctr, ok := wf.Containers[bare]; ok && ctr.Snapshot == "workspace" {
					c.errf(path, "AWF1023", catalog["AWF1023"])
				}
			}
			walkStructural(v.Body, ChildPath(parent, "map", i, "body"), wf, c, seen, scoped, mapImageTargetOwners)
		case *Compose:
			path := PathFor(parent, "compose", "", i)
			if v.As == "" || v.From == "" || v.Service == "" || len(v.Body) == 0 {
				c.errf(path, "AWF1038", catalog["AWF1038"])
			}
			if v.As != "" {
				switch {
				case strings.Contains(v.As, "{{"):
					c.errf(path, "AWF1019", catalog["AWF1019"])
				case !stepIDPattern.MatchString(v.As):
					c.errf(path, "AWF1038", fmt.Sprintf("%s: as=%q must match %s", catalog["AWF1038"], v.As, stepIDPattern))
				case containerDeclared(wf, v.As):
					c.errf(path, "AWF1038", fmt.Sprintf("%s: as=%q collides with top-level container", catalog["AWF1038"], v.As))
				case scoped[v.As]:
					c.errf(path, "AWF1038", fmt.Sprintf("%s: as=%q collides with an outer scoped handle", catalog["AWF1038"], v.As))
				}
			}
			if v.Service != "" {
				checkFieldSize(string(v.Service), path+".service", c)
			}
			nextScoped := cloneScoped(scoped)
			if v.As != "" {
				nextScoped[v.As] = true
			}
			walkStructural(v.Body, ChildPath(parent, "compose", i, "body"), wf, c, seen, nextScoped, mapImageTargetOwners)
		}
	}
}

// stepIDPattern is the allowed step id charset. Phase 3 slice 3.3 tightening (AWF1020):
// unrestricted step ids could collide with the runtime path addressing scheme — a step id
// like `generate` or containing a `.` / `[` would confuse engine.Scope.enclosingGateForEvaluate
// (which segment-matches against reserved gate tokens) and shadow keywords in journal keys
// and OTel `awf.node.path`. Restricting the charset pre-empts the collision and aligns with
// every other workflow system (Step Functions, Argo, GitHub Actions all constrain identifier
// charset). The AWF standard §2 doesn't pin allowed characters; this is a unilateral runtime
// tightening.
var stepIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// reservedStepIDTokens are control-keyword path segments the runtime addressing scheme uses;
// a step id equal to one of these would shadow the keyword in a runtime path. The charset
// rule above (no brackets, no dots) already excludes `gate[N]`, `attempt-N`, `iter-N`,
// `item-N`. This catches the single-token shadows.
var reservedStepIDTokens = map[string]bool{
	"generate": true, "evaluate": true, "until": true,
	"then": true, "else": true, "body": true,
	"do": true, "catch": true, "finally": true,
}

func checkStepID(id, path string, c *collector, seen map[string]string) {
	checkAddressableID(id, path, "step", c, seen)
}

func checkMapID(m *Map, path string, c *collector, seen map[string]string) {
	if m == nil || m.ID == "" {
		return
	}
	checkAddressableID(m.ID, path, "map aggregate", c, seen)
	if !MapProductShape(path) {
		c.errf(path, "AWF5011", catalog["AWF5011"])
	}
	if m.Reduce == nil {
		if _, _, ok := MapCompactProducer(m); !ok {
			c.errf(path, "AWF5009", catalog["AWF5009"])
		}
	}
}

func checkAddressableID(id, path, kind string, c *collector, seen map[string]string) {
	if id == "" {
		return // empty step ids surface elsewhere; empty map ids mean "no named aggregate product."
	}
	if !stepIDPattern.MatchString(id) {
		c.errf(path, "AWF1020", fmt.Sprintf("%s: id=%q (must match %s)",
			catalog["AWF1020"], id, stepIDPattern))
		// Fall through to uniqueness — a bad-charset id may still duplicate, and emitting both
		// diagnostics gives the author the full picture.
	}
	if reservedStepIDTokens[id] {
		c.errf(path, "AWF1020", fmt.Sprintf("%s: id=%q collides with reserved control keyword",
			catalog["AWF1020"], id))
	}
	if prev, dup := seen[id]; dup {
		if kind == "step" {
			c.errf(path, "AWF1004", fmt.Sprintf("%s (first seen at %s)", catalog["AWF1004"], prev))
			return
		}
		c.errf(path, "AWF1004", fmt.Sprintf("%s (%s id %q first seen at %s)", catalog["AWF1004"], kind, id, prev))
		return
	}
	seen[id] = path
}

func checkAssetID(id string, c *collector) {
	path := "assets." + id
	if !stepIDPattern.MatchString(id) {
		c.errf(path, "AWF1020", fmt.Sprintf("%s: id=%q (must match %s)",
			catalog["AWF1020"], id, stepIDPattern))
	}
	if reservedStepIDTokens[id] {
		c.errf(path, "AWF1020", fmt.Sprintf("%s: id=%q collides with reserved control keyword",
			catalog["AWF1020"], id))
	}
}

func checkContainerRefInScope(name, path string, wf *Workflow, scoped map[string]bool, mapImageTargetOwners map[string][]string, c *collector, required bool) {
	if name == "" {
		if required {
			c.errf(path, "AWF1009", fmt.Sprintf("%s (container reference is empty)", catalog["AWF1009"]))
		}
		return
	}
	// `{{` in a container ref is the AWF1019 case (handled at the call site for clearer code);
	// the resolve check would also fail here, but the message would be the wrong one.
	if strings.Contains(name, "{{") {
		return // caller emits AWF1019.
	}
	bare := bareContainerName(name)
	if scoped[bare] {
		return
	}
	if _, ok := wf.Containers[bare]; !ok {
		c.errf(path, "AWF1009", fmt.Sprintf("%s (container %q)", catalog["AWF1009"], name))
		return
	}
	checkMapImageTargetRef(path, bare, mapImageTargetOwners, c)
}

func bareContainerName(name string) string {
	if i := strings.Index(name, ":"); i >= 0 {
		return name[:i]
	}
	return name
}

func checkMapImageTargetRef(path, bare string, owners map[string][]string, c *collector) {
	for _, owner := range owners[bare] {
		if path == owner || pathWithinScope(path, owner+".body") {
			return
		}
	}
	if len(owners[bare]) > 0 {
		c.errf(path, "AWF1039", fmt.Sprintf("%s: container %q", catalog["AWF1039"], bare))
	}
}

func cloneScoped(scoped map[string]bool) map[string]bool {
	out := make(map[string]bool, len(scoped)+1)
	for k, v := range scoped {
		out[k] = v
	}
	return out
}

func containerDeclared(wf *Workflow, name string) bool {
	_, ok := wf.Containers[name]
	return ok
}

func checkParallelDistinctContainers(children NodeList, path string, c *collector) {
	// §5.4: "branches that run steps MUST target distinct containers / compose projects."
	// Walk each branch's FIRST step and collect the container ref's BARE name (left of any
	// colon — `lab:db` and `lab` both refer to the same compose project per AWF §3). Report
	// a single AWF1010 per duplicate pair so the diagnostic count doesn't explode.
	used := map[string][]int{} // bare container name → branch indices using it
	for i, child := range children {
		ctr := firstContainerRef(child)
		if ctr == "" {
			continue
		}
		bare := ctr
		if j := strings.Index(ctr, ":"); j >= 0 {
			bare = ctr[:j]
		}
		used[bare] = append(used[bare], i)
	}
	for ctr, branches := range used {
		if len(branches) > 1 {
			c.errf(path, "AWF1010", fmt.Sprintf("%s: container %q used by branches %v", catalog["AWF1010"], ctr, branches))
		}
	}
}

// NOTE: not a NodeList walker — a first-child-only descent that returns a string
// and computes no paths; not expressible as an ir.WalkNodes visit.
//
// firstContainerRef returns the container name referenced by a node's first step descendent,
// or "" if the node is a pure control structure with no step. Used by the parallel-distinct
// check to determine which container each branch is bound to.
func firstContainerRef(n Node) string {
	switch v := n.(type) {
	case *CodeStep:
		return v.Container
	case *AgentStep:
		return v.Container
	case *If:
		if len(v.Then) > 0 {
			return firstContainerRef(v.Then[0])
		}
		return ""
	case *Loop:
		if len(v.Body) > 0 {
			return firstContainerRef(v.Body[0])
		}
		return ""
	case *Try:
		if len(v.Do) > 0 {
			return firstContainerRef(v.Do[0])
		}
		return ""
	case *Gate:
		if len(v.Generate) > 0 {
			return firstContainerRef(v.Generate[0])
		}
		return ""
	case *Map:
		return v.Container
	case *Compose:
		return ""
	case *Parallel:
		// Nested parallel: take the first branch's first container.
		if len(v.Children) > 0 {
			return firstContainerRef(v.Children[0])
		}
		return ""
	}
	return ""
}

func nodeHasOutputSchema(n Node) bool {
	switch v := n.(type) {
	case *CodeStep:
		return v.OutputSchema != nil
	case *AgentStep:
		return v.OutputSchema != nil
	case *SignalStep:
		return v.OutputSchema != nil
	}
	return false
}

// checkFieldSize emits AWF1016 if src exceeds maxExpressionBytes. Applied to every Expr and
// Template field the validator parses — adversarial workflows could otherwise OOM the parser
// with a multi-megabyte condition.
func checkFieldSize(src, path string, c *collector) {
	if len(src) > maxExpressionBytes {
		c.errf(path, "AWF1016", fmt.Sprintf("%s (limit %d bytes, got %d bytes)", catalog["AWF1016"], maxExpressionBytes, len(src)))
	}
}

// checkWhereExpr validates a signal step's where: clause (SP4 keyed signals).
// The clause is template-then-expr: `{{ … }}` slots render from the engine scope
// at runtime, and bare identifiers resolve against the delivered payload. To
// validate the EXPR grammar (bounded boolean — no arithmetic) we scan the slots
// first (catches `{{ }}` imbalance), replace each with a parse-safe placeholder,
// then ParseExpr the remainder. Bare idents are NOT cross-checked against any
// output_schema (they are payload fields, not step.<id>.<field> refs). Emits
// AWF1036 on any deviation. Empty where → no-op.
//
// The placeholder is a bare `0` so it is a valid primary in BOTH operand
// positions an author may write the slot in: inside the author's own quotes for
// a string correlation value (`candidate_id == "{{ id }}"` → `candidate_id ==
// "0"`, a string literal) and bare for a numeric one (`count == {{ n }}` →
// `count == 0`, a number literal). A self-quoted placeholder would break the
// quoted-string case by colliding with the author's surrounding `"`.
func checkWhereExpr(src, path string, c *collector) {
	if src == "" {
		return
	}
	checkFieldSize(src, path, c) // AWF1016 size guard, same as other expr fields
	slots, err := template.Slots(src)
	if err != nil {
		c.errf(path, "AWF1036", fmt.Sprintf("%s: %s", catalog["AWF1036"], syntaxMessage(err)))
		return
	}
	// Replace each slot span with a placeholder primary so the surrounding
	// expression grammar parses without the runtime-substituted value. Slots are
	// in ascending Start order (template.Slots emits them left-to-right).
	var b strings.Builder
	cursor := 0
	for _, sl := range slots {
		b.WriteString(src[cursor:sl.Start])
		b.WriteString("0") // a bare number literal — valid primary in any operand position
		cursor = sl.End
	}
	b.WriteString(src[cursor:])
	if _, err := template.ParseExpr(template.UnwrapEnvelope(b.String())); err != nil {
		c.errf(path, "AWF1036", fmt.Sprintf("%s: %s", catalog["AWF1036"], syntaxMessage(err)))
	}
}

// MapImageTargets returns the bare container name of every `map` whose `image:`
// supplies a runtime per-element image (P6a). Such a container may declare
// resources alone — its image arrives per-element at dispatch. The runtime's
// capability guard (cli) also uses this to detect a runtime-image workflow.
// Recurses through every step-bearing control kind.
func MapImageTargets(wf *Workflow) map[string]bool {
	return mapImageTargetsFromOwners(MapImageTargetOwners(wf))
}

func mapImageTargetsFromOwners(owners map[string][]string) map[string]bool {
	set := map[string]bool{}
	for name := range owners {
		set[name] = true
	}
	return set
}

func MapImageTargetOwners(wf *Workflow) map[string][]string {
	owners := map[string][]string{}
	if wf == nil {
		return owners
	}
	collectMapImageTargetOwners(wf.Graph, "", owners)
	return owners
}

func collectMapImageTargetOwners(nodes NodeList, parent string, owners map[string][]string) {
	for i, n := range nodes {
		switch v := n.(type) {
		case *Map:
			if v.Image != "" && v.Container != "" {
				path := PathFor(parent, "map", "", i)
				owners[bareContainerName(v.Container)] = append(owners[bareContainerName(v.Container)], path)
			}
			collectMapImageTargetOwners(v.Body, ChildPath(parent, "map", i, "body"), owners)
		case *Compose:
			collectMapImageTargetOwners(v.Body, ChildPath(parent, "compose", i, "body"), owners)
		case *If:
			collectMapImageTargetOwners(v.Then, ChildPath(parent, "if", i, "then"), owners)
			collectMapImageTargetOwners(v.Else, ChildPath(parent, "if", i, "else"), owners)
		case *Loop:
			collectMapImageTargetOwners(v.Body, ChildPath(parent, "loop", i, "body"), owners)
		case *Try:
			collectMapImageTargetOwners(v.Do, ChildPath(parent, "try", i, "do"), owners)
			collectMapImageTargetOwners(v.Catch, ChildPath(parent, "try", i, "catch"), owners)
			collectMapImageTargetOwners(v.Finally, ChildPath(parent, "try", i, "finally"), owners)
		case *Parallel:
			collectMapImageTargetOwners(v.Children, PathFor(parent, "parallel", "", i), owners)
		case *Gate:
			collectMapImageTargetOwners(v.Generate, ChildPath(parent, "gate", i, "generate"), owners)
			collectMapImageTargetOwners(v.Evaluate, ChildPath(parent, "gate", i, "evaluate"), owners)
		}
	}
}
