import { useCallback, useEffect, useMemo, useState } from "react";
import ReactFlow, {
  Background,
  Controls,
  Handle,
  Position,
  type Edge as RFEdge,
  type Node as RFNode,
  type NodeProps,
} from "reactflow";
import "reactflow/dist/style.css";
import "./app.css";
import { layout, applyState } from "./layout";
import type { Projection } from "./projection";

interface RunRow {
  run_id: string;
  status: string;
  workflow: string;
}

// AwfNode renders every node (leaf and group). The data-* attributes are the testable
// contract the browser smoke / E2E asserts on (not colors).
function AwfNode({ data }: NodeProps) {
  const group = Boolean(data.group);
  return (
    <div
      className={`awf-node${group ? " awf-group" : ""}`}
      data-node-path={data.path}
      data-kind={data.kind}
      data-state={data.state || ""}
    >
      <Handle type="target" position={Position.Top} />
      <span className="awf-kind">{data.kind}</span>
      <span className="awf-label">{data.label}</span>
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

  // Initial load: fetch run list + static graph, lay out ONCE.
  useEffect(() => {
    (async () => {
      try {
        const rs = await getJSON<{ runs: RunRow[] }>("/api/runs");
        setRuns(rs.runs ?? []);
        const p = await getJSON<Projection>("/api/graph");
        const laid = await layout(p);
        setNodes(laid.nodes);
        setEdges(laid.edges);
        setLoaded(true);
      } catch (e) {
        setErr(String(e));
      }
    })();
  }, []);

  // refresh re-fetches the selected run's overlay and RESTYLES only (no relayout).
  const refresh = useCallback(async () => {
    try {
      const q = run ? `?run=${encodeURIComponent(run)}` : "";
      const p = await getJSON<Projection>(`/api/graph${q}`);
      setNodes((prev) => applyState(prev, p.run_overlay));
      setErr("");
    } catch (e) {
      setErr(String(e));
    }
  }, [run]);

  // Re-overlay whenever the selected run changes (structure is identical -> restyle).
  useEffect(() => {
    if (loaded) void refresh();
  }, [run, loaded, refresh]);

  const onSelect = (e: React.ChangeEvent<HTMLSelectElement>) =>
    setRun(e.target.value);

  const toolbar = useMemo(
    () => (
      <div className="awf-toolbar">
        <span className="awf-title">awf graph</span>
        <select aria-label="run" value={run} onChange={onSelect}>
          <option value="">(static — no run)</option>
          {runs.map((r) => (
            <option key={r.run_id} value={r.run_id}>
              {r.run_id.slice(0, 12)} · {r.status}
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
          fitView
          proOptions={{ hideAttribution: true }}
        >
          <Background />
          <Controls />
        </ReactFlow>
      </div>
    </div>
  );
}
