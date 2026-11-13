// Activity: the bounded recent-request ring, grouped by hour. Every group and
// every row folds, so a long day stays scannable; problems are badged.
import { useMemo } from "react";
import { SearchField, ToggleButton, ToggleButtonGroup } from "@heroui/react";
import { ChevronDown, Info, X } from "lucide-react";
import { useQueryParam } from "../api";
import { OutcomeBadge, RequestDetail, fmtClock, isFailed, isTyped, sourceIcon, useChat } from "../chat";
import { PageFrame } from "../shell";
import type { Request } from "../types";
import { Badge, Empty, Fold, Notice, Section, cn, foldOpen } from "../ui";

const FILTERS = ["all", "voice", "typed", "problems"] as const;
type Filter = (typeof FILTERS)[number];
const FILTER_LABEL: Record<Filter, string> = { all: "All", voice: "Voice", typed: "Typed", problems: "Problems" };
const isProblem = (r: Request) => isFailed(r) || r.Outcome === "cancelled" || !!r.Error;

export function ActivityPage() {
  const { view, requests, loadError } = useChat();
  const [q, setQ] = useQueryParam("q");
  const [f, setF] = useQueryParam("filter", "all");
  const filter = (FILTERS as readonly string[]).includes(f) ? (f as Filter) : "all";

  const buckets = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = [...requests]
      .reverse()
      .filter((r) =>
        filter === "voice" ? !isTyped(r.Source) : filter === "typed" ? isTyped(r.Source) : filter === "problems" ? isProblem(r) : true,
      )
      .filter((r) => !needle || `${r.Transcript} ${r.Reply} ${r.Tool} ${r.PathLabel} ${r.Error}`.toLowerCase().includes(needle));
    const m = new Map<string, Request[]>();
    for (const r of list) {
      const d = new Date(r.TimeISO);
      const key = d.toLocaleString([], { weekday: "short", hour: "numeric" });
      m.set(key, [...(m.get(key) ?? []), r]);
    }
    return [...m.entries()];
  }, [requests, filter, q]);

  return (
    <PageFrame id="activity">
      <Notice error={loadError} />
      {view && !view.HasActivity && <Notice error="The activity store is not wired in this deployment." />}
      <div className="sticky top-0 z-10 -mx-4 mb-3 flex flex-wrap items-center gap-2 bg-background/95 px-4 pt-1 pb-2 backdrop-blur md:static md:mx-0 md:px-0">
        <SearchField aria-label="Search activity" value={q} onChange={setQ} className="min-w-0 flex-1 basis-60">
          <SearchField.Group>
            <SearchField.SearchIcon />
            <SearchField.Input placeholder="Search requests, replies or tools" />
            <SearchField.ClearButton />
          </SearchField.Group>
        </SearchField>
        <ToggleButtonGroup
          size="sm"
          selectionMode="single"
          disallowEmptySelection
          selectedKeys={new Set([filter])}
          onSelectionChange={(k) => setF(String([...k][0]))}
          aria-label="Filter"
        >
          {FILTERS.map((x, i) => (
            <ToggleButton key={x} id={x}>
              {i > 0 && <ToggleButtonGroup.Separator />}
              {FILTER_LABEL[x]}
            </ToggleButton>
          ))}
        </ToggleButtonGroup>
      </div>

      <div className="space-y-3">
        {buckets.map(([hour, rs], i) => {
          const bad = rs.filter(isFailed).length;
          return (
            <Section
              key={hour}
              title={hour}
              defaultOpen={i === 0 || !!q}
              flush
              summary={`${rs.length} request${rs.length > 1 ? "s" : ""}`}
              right={bad ? <Badge tone="bad">{bad} failed</Badge> : undefined}
            >
              <ul className="divide-y divide-separator">
                {rs.map((r) => (
                  <li key={r.ID}>
                    <ActivityRow r={r} />
                  </li>
                ))}
              </ul>
            </Section>
          );
        })}
        {view && !buckets.length && (
          <div className="border">
            {q || filter !== "all" ? (
              <p className="flex items-center justify-center gap-2 py-10 text-sm text-muted">
                Nothing matches{q ? ` “${q}”` : " this filter"}.
                <button
                  onClick={() => {
                    setQ("");
                    setF("all");
                  }}
                  className="flex items-center gap-1 text-foreground"
                >
                  <X className="size-3.5" /> Clear
                </button>
              </p>
            ) : (
              <Empty>No requests yet. They appear here as soon as one completes.</Empty>
            )}
          </div>
        )}
      </div>

      {view && (
        <Section title="About this list" className="mt-6" right={<Info className="size-3.5 text-muted" />}>
          <div className="space-y-2 px-2 py-1.5 text-xs leading-relaxed text-muted">
            <p>Shows the last {view.DisplayLimit} requests. {view.CollectionNote}</p>
            <p>{view.LatencyNotice}</p>
            <p>{view.PrivacyNotice}</p>
          </div>
        </Section>
      )}
    </PageFrame>
  );
}

function ActivityRow({ r }: { r: Request }) {
  const I = sourceIcon(r.Source);
  const bad = isFailed(r);
  return (
    <Fold
      triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2 hover:bg-surface-secondary"
      bodyClassName="bg-surface-secondary px-3.5 py-3"
      trigger={
        <>
          <I className="size-4 shrink-0 text-muted" strokeWidth={1.5} aria-label={r.SourceLabel} />
          <div className="min-w-0 flex-1">
            <div className={cn("truncate text-sm", bad && "text-danger")}>{r.Transcript || <span className="text-muted">(no transcript)</span>}</div>
            <div className="truncate text-xs text-muted">{r.Reply || r.Error}</div>
          </div>
          <div className="hidden shrink-0 items-center gap-1 sm:flex">
            {r.Tool && <Badge className="font-mono">{r.Tool}</Badge>}
            <OutcomeBadge r={r} />
          </div>
          <time dateTime={r.TimeISO} className="w-16 shrink-0 text-right text-xs tabular-nums text-muted">
            {fmtClock(r.TimeISO)}
          </time>
          <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
        </>
      }
    >
      <RequestDetail r={r} showReply />
    </Fold>
  );
}
