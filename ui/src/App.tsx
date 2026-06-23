import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import ReactFlow, {
  Background,
  Controls,
  Handle,
  Position,
  type Edge as RFEdge,
  type Node as RFNode,
  type NodeProps,
  type ReactFlowInstance,
} from "reactflow";
import "reactflow/dist/style.css";
import "./app.css";
import { layout, applyState } from "./layout";
import type { Projection } from "./projection";

interface RunRow {
  run_id: string;
  status: string;
  workflow: string;
  // false when the run executed against a different version of this workflow than the file the
  // UI was launched with (the file was edited since). The run still renders faithfully against
  // its own snapshot; this only flags the version difference in the picker.
  version_match?: boolean;
}

// AwfNode renders every node (leaf and group). The data-* attributes are the testable
// contract the browser smoke / E2E asserts on (not colors).
function AwfNode({ data }: NodeProps) {
  const attrs = {
    "data-node-path": data.path,
    "data-kind": data.kind,
    "data-state": data.state || "",
    "data-node-class": data.nodeClass || "template",
    "data-live-display-class": data.liveDisplayClass || "",
    "data-live-display-tool": data.liveDisplayTool || "",
    "data-live-display-lines": data.liveDisplayLines || "",
    "data-live-display-bytes": data.liveDisplayBytes || "",
    "data-live-display-is-error": data.liveDisplayIsError ? "1" : "",
  } as const;
  // Group nodes render only a title bar at the top; their children are laid out below it
  // (ELK reserves top padding), so a child never covers the title.
  if (data.group) {
    return (
      <div className="awf-node awf-group" {...attrs}>
        <Handle type="target" position={Position.Top} />
        <div className="awf-group-head">
          <span className="awf-kind">{data.kind}</span>
          {data.label !== data.kind && <span className="awf-label">{data.label}</span>}
        </div>
        <Handle type="source" position={Position.Bottom} />
      </div>
    );
  }
  return (
    <div className="awf-node awf-leaf" {...attrs}>
      <Handle type="target" position={Position.Top} />
      <span className="awf-kind">{data.kind}</span>
      <span className="awf-label">{data.label}</span>
      {data.livePreview && <span className="awf-live-preview">{data.livePreview}</span>}
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}

const nodeTypes = { awf: AwfNode };

async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url);
  if (!r.ok) throw new Error(`${url}: ${r.status}`);
  return (await r.json()) as T;
}

export default function App() {
  const [runs, setRuns] = useState<RunRow[]>([]);
  // Initial run comes from ?run=<id> so a view is shareable (and the E2E deterministic).
  const [run, setRun] = useState<string>(
    () => new URLSearchParams(window.location.search).get("run") ?? "",
  );
  const [nodes, setNodes] = useState<RFNode[]>([]);
  const [edges, setEdges] = useState<RFEdge[]>([]);
  const [err, setErr] = useState<string>("");
  const [loaded, setLoaded] = useState(false);
  const nodeIds = useRef<Set<string>>(new Set());
  const rf = useRef<ReactFlowInstance | null>(null);

  // Re-fit the view whenever the node count changes (initial load + every relayout, e.g.
  // a run fanning out new instances). React Flow's `fitView` prop only fits once at mount,
  // which is before the async ELK layout populates nodes — so we re-fit here. The short
  // delay lets React Flow measure the freshly-rendered nodes before fitting.
  useEffect(() => {
    if (!nodes.length) return;
    const id = setTimeout(() => rf.current?.fitView({ padding: 0.16, duration: 200 }), 120);
    return () => clearTimeout(id);
  }, [nodes.length]);

  // ingest applies a projection: if the node SET is unchanged it only restyles state
  // (cheap, no relayout, preserves pan/zoom) — the common live-overlay tick. If the set
  // changed (a run added instance nodes, or the run/static selection changed) it re-runs
  // ELK layout. This keeps state ticks smooth while still growing the graph as a run
  // fans out into map items / gate attempts / loop iterations.
  const ingest = useCallback(async (p: Projection) => {
    const ids = new Set(p.nodes.map((n) => n.path));
    const cur = nodeIds.current;
    const same = ids.size === cur.size && [...ids].every((x) => cur.has(x));
    if (same) {
      setNodes((prev) => applyState(prev, p.run_overlay));
      return;
    }
    const laid = await layout(p);
    nodeIds.current = ids;
    setNodes(applyState(laid.nodes, p.run_overlay));
    setEdges(laid.edges);
  }, []);

  // Initial load: run list + static graph.
  useEffect(() => {
    (async () => {
      try {
        const rs = await getJSON<{ runs: RunRow[] }>("/api/runs");
        setRuns(rs.runs ?? []);
        await ingest(await getJSON<Projection>("/api/graph"));
        setLoaded(true);
      } catch (e) {
        setErr(String(e));
      }
    })();
  }, [ingest]);

  // Selected run -> live SSE (relayout on first/structure-changing message, restyle on
  // state-only ticks). No run -> reload the static graph.
  useEffect(() => {
    if (!loaded) return;
    if (!run) {
      getJSON<Projection>("/api/graph").then(ingest).catch((e) => setErr(String(e)));
      return;
    }
    const es = new EventSource(`/api/events?run=${encodeURIComponent(run)}`);
    es.onmessage = (ev) => {
      try {
        void ingest(JSON.parse(ev.data) as Projection);
        setErr("");
      } catch {
        /* ignore a malformed frame; the next one refreshes */
      }
    };
    es.onerror = () => setErr("live stream interrupted");
    return () => es.close();
  }, [run, loaded, ingest]);

  const refresh = useCallback(async () => {
    try {
      const q = run ? `?run=${encodeURIComponent(run)}` : "";
      await ingest(await getJSON<Projection>(`/api/graph${q}`));
      setErr("");
    } catch (e) {
      setErr(String(e));
    }
  }, [run, ingest]);

  const toolbar = useMemo(
    () => (
      <div className="awf-toolbar">
        <span className="awf-title">awf graph</span>
        <select aria-label="run" value={run} onChange={(e) => setRun(e.target.value)}>
          <option value="">(static — no run)</option>
          {runs.map((r) => (
            <option key={r.run_id} value={r.run_id}>
              {r.run_id.slice(0, 12)} · {r.status}
              {r.version_match === false ? " · ⚠ other version" : ""}
            </option>
          ))}
        </select>
        <button onClick={() => void refresh()}>Refresh</button>
        {err && <span className="awf-err">{err}</span>}
      </div>
    ),
    [runs, run, err, refresh],
  );

  return (
    <div className="awf-app">
      {toolbar}
      <div className="awf-canvas" data-loaded={loaded ? "1" : "0"}>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          onInit={(inst) => {
            rf.current = inst;
          }}
          fitView
          minZoom={0.1}
          proOptions={{ hideAttribution: true }}
        >
          <Background />
          <Controls />
        </ReactFlow>
      </div>
    </div>
  );
}
