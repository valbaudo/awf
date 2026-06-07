import ELK from "elkjs/lib/elk.bundled.js";
import type { Edge as RFEdge, Node as RFNode } from "reactflow";
import { toElkGraph, type Projection } from "./projection";

const elk = new ELK();

export interface LaidOut {
  nodes: RFNode[];
  edges: RFEdge[];
}

// layout runs ELK on the projection ONCE and maps the result to React Flow nodes/edges.
// ELK sets x/y relative to each node's parent, which is exactly React Flow's child-node
// coordinate convention (position relative to parentNode), so geometry maps directly.
// Run state is applied separately (applyState) so re-overlaying never re-lays-out.
export async function layout(p: Projection): Promise<LaidOut> {
  const meta = new Map<string, { kind: string; label: string }>();
  for (const n of p.nodes) meta.set(n.path, { kind: n.kind, label: n.id || n.kind });

  const res = (await elk.layout(toElkGraph(p) as never)) as ElkResult;

  const nodes: RFNode[] = [];
  const walk = (n: ElkResult, parent?: string) => {
    const isGroup = (n.children?.length ?? 0) > 0;
    const m = meta.get(n.id) ?? { kind: "", label: n.id };
    nodes.push({
      id: n.id,
      type: "awf",
      position: { x: n.x ?? 0, y: n.y ?? 0 },
      parentNode: parent,
      extent: parent ? "parent" : undefined,
      selectable: !isGroup,
      data: { label: m.label, kind: m.kind, path: n.id, group: isGroup, state: "" },
      style: { width: n.width, height: n.height },
    });
    for (const c of n.children ?? []) walk(c, n.id);
  };
  for (const c of res.children ?? []) walk(c, undefined);

  const edges: RFEdge[] = p.edges.map((e, i) => ({
    id: `e${i}`,
    source: e.from,
    target: e.to,
  }));
  return { nodes, edges };
}

// applyState recolors existing nodes from a run overlay WITHOUT relayout (the restyle
// path Refresh uses in 2a and SSE will reuse in 2b). Positions are untouched.
export function applyState(
  nodes: RFNode[],
  overlay: Record<string, { state: string }> | undefined,
): RFNode[] {
  return nodes.map((n) => ({
    ...n,
    data: { ...n.data, state: overlay?.[n.id]?.state ?? "" },
  }));
}

interface ElkResult {
  id: string;
  x?: number;
  y?: number;
  width?: number;
  height?: number;
  children?: ElkResult[];
}
