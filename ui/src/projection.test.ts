import { describe, expect, it } from "vitest";
import { applyState } from "./layout";
import { containerOf, countGroups, stateOf, toElkGraph, type Projection } from "./projection";

const demo: Projection = {
  schema_version: 1,
  workflow: "demo",
  nodes: [
    { path: "build", kind: "code", id: "build" },
    { path: "gate[1]", kind: "gate" },
    { path: "gate[1].generate.gen", kind: "agent", id: "gen", parent: "gate[1].generate" },
    { path: "gate[1].evaluate.check", kind: "code", id: "check", parent: "gate[1].evaluate" },
    { path: "map[2]", kind: "map" },
    { path: "map[2].body.work", kind: "code", id: "work", parent: "map[2].body" },
  ],
  edges: [
    { from: "build", to: "gate[1]", kind: "control" },
    { from: "gate[1]", to: "map[2]", kind: "control" },
  ],
};

describe("containerOf", () => {
  const set = new Set(demo.nodes.map((n) => n.path));
  it("returns undefined at top level", () => {
    expect(containerOf("", set)).toBeUndefined();
    expect(containerOf(undefined, set)).toBeUndefined();
  });
  it("strips a branch-scope parent to the container node", () => {
    expect(containerOf("gate[1].generate", set)).toBe("gate[1]");
    expect(containerOf("map[2].body", set)).toBe("map[2]");
  });
  it("uses a parent that is already a node as-is (parallel case)", () => {
    const s = new Set(["parallel[0]", "parallel[0].x"]);
    expect(containerOf("parallel[0]", s)).toBe("parallel[0]");
  });
});

describe("toElkGraph", () => {
  const g = toElkGraph(demo);
  it("nests children under their container nodes", () => {
    // roots: build, gate[1], map[2]
    expect(g.children.map((c) => c.id).sort()).toEqual(["build", "gate[1]", "map[2]"]);
    const gate = g.children.find((c) => c.id === "gate[1]")!;
    expect(gate.children.map((c) => c.id).sort()).toEqual([
      "gate[1].evaluate.check",
      "gate[1].generate.gen",
    ]);
    const map = g.children.find((c) => c.id === "map[2]")!;
    expect(map.children.map((c) => c.id)).toEqual(["map[2].body.work"]);
  });
  it("carries every edge", () => {
    expect(g.edges).toHaveLength(2);
    expect(g.edges[0]).toMatchObject({ sources: ["build"], targets: ["gate[1]"] });
  });
  it("counts groups (gate + map are containers)", () => {
    expect(countGroups(g)).toBe(2);
  });
});

describe("stateOf", () => {
  it("reads overlay state, empty when absent", () => {
    expect(stateOf({ build: { state: "completed" } }, "build")).toBe("completed");
    expect(stateOf({ build: { state: "completed" } }, "gate[1]")).toBe("");
    expect(stateOf(undefined, "build")).toBe("");
  });
});

describe("applyState", () => {
  it("carries live overlay metadata into node data", () => {
    const [node] = applyState(
      [
        {
          id: "gen",
          position: { x: 0, y: 0 },
          data: { label: "gen", kind: "agent", state: "" },
        },
      ],
      {
        gen: {
          state: "running",
          live_preview: "registry finalizer needs cleanup",
          live_display_class: "notice",
          live_display_tool: "shell",
          live_display_lines: 3,
          live_display_bytes: 42,
          live_display_is_error: true,
        },
      },
    );

    expect(node.data).toMatchObject({
      state: "running",
      livePreview: "registry finalizer needs cleanup",
      liveDisplayClass: "notice",
      liveDisplayTool: "shell",
      liveDisplayLines: 3,
      liveDisplayBytes: 42,
      liveDisplayIsError: true,
    });
  });
});
