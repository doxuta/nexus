"use client";

// RelationMap (specs/075 T149, FR-088/089): `dung/du-lieu/so-do`. One work surface in the "one-item page" mould: a header
// row ("{n} bảng · {m} liên kết" + search), a shaded stage with the xyflow canvas, and a right column with three modes
// (links / one table / one link). Reads only; removal and "how it lists" go through RelationPanel. Table positions are the
// maker's own and persist quietly per tenant in localStorage (⌘Z undoes a move; schema changes are never undoable).
// Not scoped to an app (D-UI-20): a link legitimately crosses apps. No modal anywhere.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { ReactFlow, ReactFlowProvider, applyNodeChanges, useReactFlow, type Connection, type Node, type NodeChange } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { useQuery } from "@tanstack/react-query";
import { ensureAuth, getCachedMe, type FieldMeta, type RelViewConfig } from "@/lib/api";
import { readForBuilder } from "@/lib/readForBuilder";
import { strictErrorMessage } from "@/lib/errorMessages";
import { useSurfaceWidth } from "@/components/studio/screen/useSurfaceWidth";
import { t, tf } from "@/lib/i18n";
import {
  NODE_W, dagreLayout, deriveEdges, layoutStorageKey, matchEntities, neighbourhood, nodeHeight,
  type CanvasEdgeSpec, type XY,
} from "@/lib/relCanvas";
import { studioHref } from "@/lib/studioRoutes";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { usePublishState } from "@/components/studio/usePublishState";
import { hydratePositions, useLayoutStore } from "@/components/studio/relcanvas/useLayoutStore";
import { RelationEdge, RelationMarkerDefs, type RelationEdgeType } from "./RelationEdge";
import { RelationMapToolbar } from "./RelationMapToolbar";
import { RelationNode, type RelationNodeType } from "./RelationNode";
import type { CreatedLink } from "./RelationForm";
import { RelationPanel, type PanelEntity, type RelationSelection } from "./RelationPanel";

const NODE_TYPES = { table: RelationNode };
const EDGE_TYPES = { link: RelationEdge };
/** "Vừa khung" never shrinks below this: 20+ tables stay readable and are panned instead of becoming specks. */
const FIT_MIN_ZOOM = 0.6;
const GRAPH_KEY = ["studio", "relmap", "graph"] as const;

interface Graph { entities: PanelEntity[]; edges: CanvasEdgeSpec[]; storageKey: string; positions: Record<string, XY> }

async function loadGraph(delivered: boolean): Promise<Graph> {
  await ensureAuth();
  const metas = await readForBuilder.listEntities(delivered);
  const [fieldLists, rels] = await Promise.all([
    Promise.all(metas.map((m) => readForBuilder.listFields(delivered, m.name).catch(() => [] as FieldMeta[]))),
    readForBuilder.listAllRelationships(delivered).catch(() => []),
  ]);
  const entities: PanelEntity[] = metas.map((m, i) => ({ name: m.name, label: m.label, fields: fieldLists[i] }));
  const edges = deriveEdges(new Set(metas.map((m) => m.name)), Object.fromEntries(entities.map((e) => [e.name, e.fields])), rels);
  const storageKey = layoutStorageKey(getCachedMe()?.tenantId ?? "", metas.map((m) => m.name));
  let stored: Record<string, XY> | undefined;
  try {
    const raw = window.localStorage.getItem(storageKey);
    if (raw) stored = (JSON.parse(raw) as { positions?: Record<string, XY> }).positions;
  } catch { stored = undefined; }
  const auto = dagreLayout(entities.map((e) => ({ id: e.name, width: NODE_W, height: nodeHeight(e.fields.length) })), edges);
  const positions = Object.fromEntries(entities.map((e) => [e.name, stored?.[e.name] ?? auto[e.name]]));
  hydratePositions(positions); // no undo entry, fresh history
  return { entities, edges, storageKey, positions };
}

export function RelationMap({ slug, focusEntity }: { slug: string; focusEntity?: string }) {
  const delivered = usePublishState(slug).data?.delivered === true;
  // `delivered` in the key: the first read after delivery goes over the draft layer (specs/075 T194)
  const q = useQuery({ queryKey: [...GRAPH_KEY, delivered], queryFn: () => loadGraph(delivered), retry: false, staleTime: 0, gcTime: 0 });
  const { ref: rootRef, stacked } = useSurfaceWidth();
  const [sel, setSel] = useState<RelationSelection>(null);
  const [search, setSearch] = useState("");
  const [showAll, setShowAll] = useState(false);
  const [centerReq, setCenterReq] = useState<{ name: string; nonce: number } | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const data = q.data;

  // Quiet persistence of positions, debounced; storage failures are not worth a toast.
  const storageKey = data?.storageKey;
  useEffect(() => {
    if (!storageKey) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const unsub = useLayoutStore.subscribe((s) => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        try { window.localStorage.setItem(storageKey, JSON.stringify({ v: 1, positions: s.positions })); } catch { /* quota: non-fatal */ }
      }, 400);
    });
    return () => { unsub(); clearTimeout(timer); };
  }, [storageKey]);

  const visible = useMemo(() => {
    if (!data) return { entities: [] as PanelEntity[], edges: [] as CanvasEdgeSpec[] };
    const keep = focusEntity && !showAll && data.entities.some((e) => e.name === focusEntity) ? neighbourhood(focusEntity, data.edges) : null;
    const entities = keep ? data.entities.filter((e) => keep.has(e.name)) : data.entities;
    const names = new Set(entities.map((e) => e.name));
    return { entities, edges: data.edges.filter((e) => names.has(e.source) && names.has(e.target)) };
  }, [data, focusEntity, showAll]);

  const matches = useMemo(() => (search.trim() ? new Set(matchEntities(visible.entities, search).map((e) => e.name)) : null), [visible.entities, search]);
  const reload = useCallback(() => { setSel(null); void q.refetch(); }, [q]);
  // After a create: reload, then select the new link (the 1-N field name / the N-N link name tells which one it is).
  const afterCreate = useCallback(async (c: CreatedLink) => {
    const r = await q.refetch();
    const hit = r.data?.edges.find((e) => e.source === c.parent && e.target === c.child && (c.relName ? e.rel?.name === c.relName : e.field?.name === c.fieldName));
    setSel(hit ? { kind: "edge", id: hit.id } : null);
  }, [q]);
  const labelOf = useCallback((n: string) => data?.entities.find((e) => e.name === n)?.label ?? n, [data]);

  const onSearchKey = (e: React.KeyboardEvent) => {
    if (e.key !== "Enter" || !matches || matches.size === 0) return;
    const first = [...matches][0];
    setSel({ kind: "node", id: first });
    setCenterReq({ name: first, nonce: Date.now() });
  };
  // ⌘F is taken over only while focus is inside the map; elsewhere the browser keeps its own find.
  const onGroupKey = (e: React.KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "f") {
      e.preventDefault();
      searchRef.current?.focus();
    }
  };

  let body: React.ReactNode;
  if (q.isPending) {
    body = <div style={{ position: "relative", minHeight: "8rem" }}><Spinner label={t("studio.build.relMap.state.loading")} container /></div>;
  } else if (q.isError || !data) {
    body = (
      <div>
        <ScopedNotification theme="error">{t("studio.build.relMap.state.error")} {strictErrorMessage(q.error)}</ScopedNotification>
        <div className="slds-m-top_small"><Button variant="neutral" onClick={() => void q.refetch()}>{t("studio.build.relMap.state.retry")}</Button></div>
      </div>
    );
  } else if (data.entities.length === 0) {
    body = (
      <div>
        <p className="slds-text-body_regular">{t("studio.build.relMap.state.noEntities")}</p>
        <Link className="slds-button slds-button_neutral slds-m-top_small" href={studioHref(slug, "dung/du-lieu")}>{t("studio.build.relMap.state.noEntitiesCta")}</Link>
      </div>
    );
  } else {
    const focusLabel = focusEntity && !showAll ? labelOf(focusEntity) : null;
    body = (
      <>
        <div className="flex flex-wrap items-end gap-3 slds-m-bottom_small">
          <p className="slds-text-body_regular" style={{ fontWeight: 700 }}>
            {tf("studio.build.relMap.summary", { n: visible.entities.length, m: visible.edges.length })}
          </p>
          {focusLabel && (
            <p className="slds-text-body_small slds-text-color_weak">
              {tf("studio.build.relMap.focus", { bang: focusLabel })}{" "}
              <Button variant="base" onClick={() => setShowAll(true)}>{t("studio.build.relMap.showAll")}</Button>
            </p>
          )}
          <Button variant="neutral" onClick={() => setSel({ kind: "new" })}>{t("studio.build.relMap.panel.newLink")}</Button>
          <div className="ml-auto" style={{ width: "16rem" }}>
            <Input
              ref={searchRef}
              label={t("studio.build.relMap.search")}
              labelHidden
              placeholder={t("studio.build.relMap.search")}
              value={search}
              clearable
              onClear={() => setSearch("")}
              onChange={(e) => setSearch(e.target.value)}
              onKeyDown={onSearchKey}
            />
          </div>
        </div>
        {matches && matches.size === 0 && (
          <p className="slds-text-body_small slds-m-bottom_x-small">
            {tf("studio.build.relMap.state.noMatch", { q: search.trim() })}{" "}
            <Button variant="base" onClick={() => setSearch("")}>{t("studio.build.relMap.state.clearSearch")}</Button>
          </p>
        )}
        <div style={{ display: "grid", gap: "1rem", gridTemplateColumns: stacked ? "minmax(0,1fr)" : "minmax(0,1fr) 22rem" }}>
          <div
            role="group"
            aria-label={tf("studio.build.relMap.group", { n: visible.entities.length, m: visible.edges.length })}
            onKeyDown={onGroupKey}
            className="slds-theme_shade flex flex-col"
            style={{ height: stacked ? 360 : "max(26rem, calc(100dvh - var(--nx-sticky-stack, 0px) - 9rem))" }}
          >
            <div className="flex-1 min-h-0" style={{ position: "relative" }}>
              <ReactFlowProvider>
                <Stage key={q.dataUpdatedAt + (showAll ? "a" : "f")} graph={data} entities={visible.entities} edges={visible.edges} matches={matches} sel={sel} onSelect={setSel} centerReq={centerReq} />
              </ReactFlowProvider>
            </div>
            <p className="slds-text-body_small slds-text-color_weak slds-p-horizontal_small slds-p-vertical_xx-small slds-border_top">
              {t("studio.build.relMap.legend")}
            </p>
          </div>
          <aside className={stacked ? "slds-border_top slds-p-top_small" : "slds-border_left slds-p-left_medium"}>
            <RelationPanel
              slug={slug}
              selection={sel}
              entities={visible.entities}
              edges={visible.edges}
              onSelect={setSel}
              onChanged={reload}
              onCreated={afterCreate}
              onViewConfigSaved={(relId: string, cfg: RelViewConfig | null) => {
                // keep the panel honest without a reload: the saved document is the new "initial"
                const edge = data.edges.find((e) => e.rel?.id === relId);
                if (edge?.rel) edge.rel.viewConfig = cfg ?? undefined;
              }}
            />
          </aside>
        </div>
        {data.edges.length === 0 && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.build.relMap.state.noRelations")}</p>}
      </>
    );
  }
  return <div ref={rootRef} className="flex flex-col flex-1 min-h-0 slds-p-around_medium">{body}</div>;
}

interface StageProps {
  graph: Graph;
  entities: PanelEntity[];
  edges: CanvasEdgeSpec[];
  matches: Set<string> | null;
  sel: RelationSelection;
  onSelect: (s: RelationSelection) => void;
  centerReq: { name: string; nonce: number } | null;
}

function Stage({ graph, entities, edges, matches, sel, onSelect, centerReq }: StageProps) {
  const rf = useReactFlow();
  // Base nodes hold position + measured size; selection/match are overlaid at render time.
  const [base, setBase] = useState<Node[]>(() =>
    entities.map((e) => ({
      id: e.name, type: "table", position: graph.positions[e.name] ?? { x: 0, y: 0 },
      initialWidth: NODE_W, initialHeight: nodeHeight(e.fields.length), data: {},
    })),
  );
  const [announce, setAnnounce] = useState("");

  const syncFromStore = useCallback(() => {
    const pos = useLayoutStore.getState().positions;
    setBase((ns) => ns.map((n) => (pos[n.id] ? { ...n, position: pos[n.id] } : n)));
  }, []);

  const onMove = useCallback((name: string, dx: number, dy: number) => {
    const cur = useLayoutStore.getState().positions[name];
    if (!cur) return;
    useLayoutStore.getState().moveNode(name, { x: cur.x + dx, y: cur.y + dy });
    syncFromStore();
    setAnnounce(tf("studio.build.relMap.moved", { bang: entities.find((e) => e.name === name)?.label ?? name }));
  }, [entities, syncFromStore]);

  const nodes = useMemo(
    () => base.map((n) => {
      const e = entities.find((x) => x.name === n.id);
      return {
        ...n,
        data: {
          name: n.id, label: e?.label ?? n.id, fields: e?.fields ?? [],
          selected: sel?.kind === "node" && sel.id === n.id, match: !!matches?.has(n.id),
          onSelect: (name: string) => onSelect({ kind: "node", id: name }), onMove,
        },
      } as RelationNodeType;
    }),
    [base, entities, sel, matches, onSelect, onMove],
  );
  const flowEdges = useMemo(
    () => edges.map((e) => ({
      id: e.id, source: e.source, target: e.target, type: "link", focusable: false,
      data: { relType: e.relType, selected: sel?.kind === "edge" && sel.id === e.id, parallelIndex: e.parallelIndex },
    }) as RelationEdgeType),
    [edges, sel],
  );

  const onNodesChange = useCallback((changes: NodeChange[]) => setBase((ns) => applyNodeChanges(changes, ns)), []);

  const relayout = () => {
    const pos = dagreLayout(entities.map((e) => ({ id: e.name, width: NODE_W, height: nodeHeight(e.fields.length) })), edges);
    useLayoutStore.getState().setPositions({ ...useLayoutStore.getState().positions, ...pos });
    syncFromStore();
    requestAnimationFrame(() => void rf.fitView({ padding: 0.18, minZoom: FIT_MIN_ZOOM, duration: 250 }));
    toast.info(t("studio.build.relMap.tool.relayoutDone"));
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== "z") return;
    e.preventDefault();
    const tm = useLayoutStore.temporal.getState();
    if (e.shiftKey) tm.redo(); else tm.undo();
    syncFromStore();
  };

  useEffect(() => {
    if (!centerReq) return;
    const n = rf.getNode(centerReq.name);
    if (n) void rf.setCenter(n.position.x + NODE_W / 2, n.position.y + 40, { zoom: Math.max(rf.getZoom(), 0.9), duration: 250 });
  }, [centerReq, rf]);

  return (
    <div style={{ position: "absolute", inset: 0 }} onKeyDown={onKeyDown}>
      <RelationMarkerDefs />
      <ReactFlow
        nodes={nodes}
        edges={flowEdges}
        nodeTypes={NODE_TYPES}
        edgeTypes={EDGE_TYPES}
        onNodesChange={onNodesChange}
        onNodeDragStop={(_, n) => useLayoutStore.getState().moveNode(n.id, n.position)}
        onEdgeClick={(_, e) => onSelect({ kind: "edge", id: e.id })}
        onPaneClick={() => onSelect(null)}
        nodesConnectable
        onConnect={(c: Connection) => c.source !== c.target && onSelect({ kind: "new", parent: c.source, child: c.target })}
        nodesFocusable={false}
        edgesFocusable={false}
        fitView
        fitViewOptions={{ padding: 0.18, minZoom: FIT_MIN_ZOOM }}
        minZoom={0.2}
        maxZoom={1.6}
        proOptions={{ hideAttribution: true }}
      />
      <div style={{ position: "absolute", left: "0.75rem", top: "0.75rem" }}>
        <RelationMapToolbar
          onZoomIn={() => void rf.zoomIn({ duration: 150 })}
          onZoomOut={() => void rf.zoomOut({ duration: 150 })}
          onFit={() => void rf.fitView({ padding: 0.18, minZoom: FIT_MIN_ZOOM, duration: 250 })}
          onRelayout={relayout}
        />
      </div>
      <p className="slds-assistive-text" aria-live="polite">{announce}</p>
    </div>
  );
}
