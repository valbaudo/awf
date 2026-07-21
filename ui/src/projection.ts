// Projection is the JSON contract emitted by `awf graph --json` (the Slice-1 graph
// package) and served by the awf ui server at /api/graph. Pure transforms here turn it
// into an ELK layout graph and apply run state; the async layout step lives in layout.ts.

export interface PNode {
  path: string;
  kind: string;
  id?: string;
  parent?: string;
  with?: Record<string, unknown>;
  node_class?: string; // "template" | "instance"
  instance_of?: string;
}
export interface PEdge {
  from: string;
  to: string;
  kind: string;
}
export interface NodeState {
  state: string;
  outcome?: string;
  live_preview?: string;
  live_display_class?: string;
  live_display_tool?: string;
  live_display_lines?: number;
  live_display_bytes?: number;
  live_display_is_error?: boolean;
}
export interface Projection {
  schema_version: number;
  workflow: string;
  nodes: PNode[];
  edges: PEdge[];
  run_overlay?: Record<string, NodeState>;
}

export interface ElkNode {
  id: string;
  width?: number;
  height?: number;
  labels?: { text: string }[];
  children: ElkNode[];
  layoutOptions?: Record<string, string>;
  // awf metadata carried through layout so the React Flow mapping can read it back.
  awf?: { kind: string; container?: string };
}
export interface ElkEdge {
  id: string;
  sources: string[];
  targets: string[];
}
export interface ElkGraph {
  id: string;
  layoutOptions: Record<string, string>;
  children: ElkNode[];
  edges: ElkEdge[];
}

export const NODE_W = 156;
export const NODE_H = 64;

// Padding inside a container node. The top reserves the group title bar (28px, see
// .awf-group-head) so a child never covers the group's own title; the rest gives
// nested panels breathing room.
//
// elk.padding is a PER-NODE property, NOT inherited from the root graph's
// layoutOptions. A container left without it silently gets ELK's 12px default —
// less than the title bar — and its first child overlaps the title. So every node
// that has children carries this explicitly.
export const GROUP_PADDING = "[top=40,left=18,bottom=18,right=18]";

// containerOf returns the React-Flow / ELK parent of a node: the nearest ancestor that
// is itself a node. The projection's `parent` is the enclosing ADDRESSING SCOPE, which
// may be a branch-scope path (e.g. "gate[1].generate", "loop[0].body") that is NOT a
// node. A parent that IS a node (parallel children carry the bare "parallel[i]" node
// path) is used as-is; otherwise strip the trailing ".<branch>" segment to reach the
// container node. Returns undefined for top-level nodes.
//
// (This scope->node mapping is the seam the deferred runtime-instance object model will
// formalize; for the static graph it is a small pure function. See TODOS.md.)
export function containerOf(
  parent: string | undefined,
  nodeSet: Set<string>,
): string | undefined {
  if (!parent) return undefined;
  if (nodeSet.has(parent)) return parent;
  const i = parent.lastIndexOf(".");
  if (i < 0) return undefined;
  const stripped = parent.slice(0, i);
  return nodeSet.has(stripped) ? stripped : undefined;
}

// toElkGraph builds the hierarchical ELK input from a projection. Pure and deterministic
// (preserves projection node/edge order). Container nodes hold their children; edges sit
// at the root with INCLUDE_CHILDREN so ELK routes across the hierarchy.
export function toElkGraph(p: Projection): ElkGraph {
  const set = new Set(p.nodes.map((n) => n.path));
  const byId = new Map<string, ElkNode>();
  for (const n of p.nodes) {
    byId.set(n.path, {
      id: n.path,
      width: NODE_W,
      height: NODE_H,
      labels: [{ text: n.id || n.kind }],
      children: [],
      awf: { kind: n.kind, container: containerOf(n.parent, set) },
    });
  }
  const roots: ElkNode[] = [];
  for (const n of p.nodes) {
    const node = byId.get(n.path)!;
    const c = node.awf!.container;
    if (c && byId.has(c)) byId.get(c)!.children.push(node);
    else roots.push(node);
  }
  // A gate attempt's generate and evaluate scopes have no control edge between them,
  // so ELK is free to order them arbitrarily and falls back to input order — which is
  // sorted by path, putting "evaluate" before "generate" and rendering the judge ahead
  // of the generator that feeds it. Order the two scopes explicitly. Everything else
  // keeps projection order (Array.sort is stable), and containers without these scopes
  // are untouched.
  const scopeRank = (n: ElkNode) =>
    n.awf?.kind === "generate" ? 0 : n.awf?.kind === "evaluate" ? 1 : 2;
  for (const n of byId.values()) {
    if (n.children.some((c) => c.awf?.kind === "generate" || c.awf?.kind === "evaluate")) {
      n.children.sort((a, b) => scopeRank(a) - scopeRank(b));
    }
    if (n.children.length > 0) n.layoutOptions = { "elk.padding": GROUP_PADDING };
  }
  // Only CONTROL edges drive layout; data edges (cross-cutting {{ }} references) are
  // rendered but excluded here so they cannot distort the layered/nested layout.
  const edges: ElkEdge[] = p.edges
    .filter((e) => e.kind !== "data")
    .map((e, i) => ({ id: `c${i}`, sources: [e.from], targets: [e.to] }));
  return {
    id: "root",
    layoutOptions: {
      "elk.algorithm": "layered",
      "elk.direction": "DOWN",
      "elk.hierarchyHandling": "INCLUDE_CHILDREN",
      "elk.layered.spacing.nodeNodeBetweenLayers": "48",
      "elk.spacing.nodeNode": "26",
      "elk.spacing.edgeNode": "20",
      "elk.padding": GROUP_PADDING,
    },
    children: roots,
    edges,
  };
}

// countGroups returns how many nodes are containers (have >=1 child) in the ELK graph —
// used by tests and by the renderer to style group nodes.
export function countGroups(g: ElkGraph): number {
  let n = 0;
  const walk = (node: ElkNode) => {
    if (node.children.length > 0) n++;
    node.children.forEach(walk);
  };
  g.children.forEach(walk);
  return n;
}

// stateOf returns the run state for a node path from the overlay, or "" if none.
export function stateOf(
  overlay: Record<string, NodeState> | undefined,
  path: string,
): string {
  return overlay?.[path]?.state ?? "";
}
