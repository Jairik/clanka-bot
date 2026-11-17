// Activity: the request history, newest first and grouped by hour. The first
// page is the live ring for an instant view; older pages and searches read the
// assistant's durable history, one bounded page at a time.
import { useEffect, useMemo, useState } from "react";
import { Button, SearchField, ToggleButton, ToggleButtonGroup } from "@heroui/react";
import { ChevronDown, Info, X } from "lucide-react";
import { getJSON, usePageData, useQueryParam } from "../api";
import { activityQuery, asFilter, bucketActivity, filterActivity, FILTERS, FILTER_LABEL } from "../activity";
import { OutcomeBadge, RequestDetail, fmtClock, isFailed, sourceIcon, useChat } from "../chat";
import { PageFrame } from "../shell";
import { FocusStatus, emptyFocus } from "../focus";
import type { ActivityView, LatencyView, Request } from "../types";
import { Badge, Empty, Fold, Notice, Section, cn, foldOpen } from "../ui";

type Page = { key: string; list: Request[]; next: string; notice: string };

export function ActivityPage() {
  const { view, requests, loadError } = useChat();
  // The focus and announcement state, refreshed live by the "focus" SSE delta.
  const { data: focus } = usePageData("focus");
  const [q, setQ] = useQueryParam("q");
  // Keep the field value in component state and flush it to the URL on a 250 ms debounce.
  const [draft, setDraft] = useState(q);

  // Synchronize draft with the query param on external navigation (e.g. Back/Forward).
  const [prevQ, setPrevQ] = useState(q);
  if (q !== prevQ) {
    setPrevQ(q);
    setDraft(q);
  }

  // Typing must not be one history write per character; the URL catches up once.
  useEffect(() => {
    if (draft === q) return;
    const id = setTimeout(() => setQ(draft), 250);
    return () => clearTimeout(id);
  }, [draft, q, setQ]);

  /** Flushes the draft search query to the URL query param immediately. */
  const flush = () => {
    if (draft !== q) setQ(draft);
  };

  const [f, setF] = useQueryParam("filter", "all");
  const filter = asFilter(f);
  const key = `${filter}\u0000${q.trim()}`;
  const active = filter !== "all" || !!q.trim();
  // A ring-only deployment (a smoke stub, or history not wired) filters in
  // memory exactly as before; a durable one asks the server so a search can
  // reach last week.
  const durable = view?.HasHistory ?? false;
  const server = durable && active;
  const [feed, setFeed] = useState<Page | null>(null);
  const [older, setOlder] = useState<Page | null>(null);
  const [error, setError] = useState("");
  const [loadingOlder, setLoadingOlder] = useState(false);

  useEffect(() => {
    setOlder(null);
    if (!server) {
      setFeed(null);
      setError("");
      return;
    }
    let cancelled = false;
    // Debounce a typed query so one request is sent after the pause, not one
    // per keystroke.
    const timer = setTimeout(() => {
      getJSON<ActivityView>(activityQuery({ filter, q })).then(
        (v) => {
          if (cancelled) return;
          setFeed({ key, list: v.Requests ?? [], next: v.Next, notice: v.SearchNotice });
          setError("");
        },
        (e) => !cancelled && setError(e instanceof Error ? e.message : String(e)),
      );
    }, 200);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [key, server, filter, q]);

  const searching = server && feed?.key !== key;
  // The fetched page carries the store's own account of how far a search
  // reached. An unfiltered page and a ring-only deployment have no window to
  // declare, so the notice is empty there even if a previous query had one.
  const searchNotice = server && feed?.key === key ? feed.notice : "";
  const base = server ? (feed?.key === key ? feed.list : []) : [...requests].reverse();
  const olderList = older?.key === key ? older.list : [];
  // Newest first, filtered once more so a stub that ignores the query still
  // behaves like the old in-memory page.
  const list = filterActivity([...base, ...olderList], filter, q);
  // The plain page also pages through `older`: the first cursor comes from the
  // live view, and every loaded page advances it. Reading only view.Next here
  // refetched page 2 on every click.
  const next = older?.key === key ? older.next : server && feed?.key === key ? feed.next : server ? "" : (view?.Next ?? "");
  const buckets = useMemo(() => bucketActivity(list), [list]);

  const loadOlder = async () => {
    if (!next) return;
    setLoadingOlder(true);
    try {
      const page = await getJSON<ActivityView>(activityQuery({ filter, q, before: next }));
      setOlder((prev) =>
        prev && prev.key === key
          ? { key, list: [...prev.list, ...(page.Requests ?? [])], next: page.Next, notice: page.SearchNotice }
          : { key, list: page.Requests ?? [], next: page.Next, notice: page.SearchNotice },
      );
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingOlder(false);
    }
  };

  return (
    <PageFrame id="activity">
      <Notice error={loadError || error} />
      {view && !view.HasActivity && <Notice error="The activity store is not wired in this deployment." />}
      <div className="sticky top-0 z-10 -mx-4 mb-3 flex flex-wrap items-center gap-2 bg-background/95 px-4 pt-1 pb-2 backdrop-blur md:static md:mx-0 md:px-0">
        <SearchField
          aria-label="Search activity"
          value={draft}
          onChange={setDraft}
          onSubmit={flush}
          onClear={() => {
            setDraft("");
            setQ("");
          }}
          className="min-w-0 flex-1 basis-60"
        >
          <SearchField.Group>
            <SearchField.SearchIcon />
            <SearchField.Input
              placeholder={durable ? "Search all activity" : "Search requests, replies or tools"}
              onBlur={flush}
              onKeyDown={(e) => {
                if (e.key === "Enter") flush();
              }}
            />
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
        <div className="w-full">
          <FocusStatus view={focus ?? emptyFocus()} />
        </div>
      </div>
      {searchNotice && (
        <p className="flex items-start gap-1.5 pb-2 text-xs text-muted">
          <Info className="mt-0.5 size-3.5 shrink-0" />
          {searchNotice}
        </p>
      )}

      <div className="space-y-3" aria-busy={searching || loadingOlder}>
        <LatencyPanel />
        {searching && <p className="py-3 text-center text-sm text-muted">Searching the history…</p>}
        {buckets.map(({ key: bucketKey, title, list: rs }, i) => {
          const bad = rs.filter(isFailed).length;
          return (
            <Section
              key={bucketKey}
              title={title}
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
        {view && !buckets.length && !searching && (
          <div className="border">
            {q || filter !== "all" ? (
              <p className="flex items-center justify-center gap-2 py-10 text-sm text-muted">
                Nothing matches{q ? ` “${q}”` : " this filter"}
                {searchNotice ? " in the searched window" : ""}.
                <button
                  onClick={() => {
                    setDraft("");
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
        {next && !searching && (
          <div className="flex justify-center">
            <Button size="sm" variant="secondary" onPress={loadOlder} isDisabled={loadingOlder}>
              {loadingOlder ? "Loading older requests…" : "Load older"}
            </Button>
          </div>
        )}
      </div>

      {view && (
        <Section title="About this list" className="mt-6" right={<Info className="size-3.5 text-muted" />}>
          <div className="space-y-2 px-2 py-1.5 text-xs leading-relaxed text-muted">
            <p>Shows up to {view.DisplayLimit} requests per page. {view.CollectionNote}</p>
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
  // A browser-mic request is the only one with an STT measurement on the row;
  // that number is why the Mic filter exists.
  const mic = r.Source === "dashboard_mic";
  const stt = mic ? (r.Stages ?? []).find((s) => s.Name === "stt_finalize") : undefined;
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
            {mic && (
              <div className="mt-1 flex flex-wrap items-center gap-1.5 text-xs text-muted">
                <Badge>{r.SourceLabel}</Badge>
                {stt && <span>STT {stt.MS}</span>}
              </div>
            )}
          </div>
          <div className="hidden shrink-0 items-center gap-1 sm:flex">
            {r.Summary && <Badge>Summary</Badge>}
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

// formatLatencyMS formats milliseconds into a readable label (e.g. "85 ms" or "1.2 s").
function formatLatencyMS(ms: number): string {
  if (ms < 1000) {
    return `${Math.round(ms)} ms`;
  }
  return `${(ms / 1000).toFixed(1)} s`;
}

// LatencyPanel renders the per-stage latency percentiles when opened by the user.
export function LatencyPanel() {
  const [open, setOpen] = useState(false);
  const [window, setWindow] = useState<"24h" | "7d">("24h");
  const [data, setData] = useState<LatencyView | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) return;
    setLoading(true);
    setError("");
    let cancelled = false;
    getJSON<LatencyView>(`/api/ui/latency?window=${window}`).then(
      (res) => {
        if (cancelled) return;
        setData(res);
        setLoading(false);
      },
      (err) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      },
    );
    return () => {
      cancelled = true;
    };
  }, [open, window]);

  return (
    <Section
      title="Latency percentiles"
      isExpanded={open}
      onExpandedChange={setOpen}
      summary={open && data ? `${data.records} requests` : undefined}
      className="mb-3"
      flush
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-separator px-3.5 py-2">
        <span className="text-xs text-muted">
          {data ? `${data.records} request${data.records === 1 ? "" : "s"} in window` : "Window"}
        </span>
        <ToggleButtonGroup
          size="sm"
          selectionMode="single"
          disallowEmptySelection
          selectedKeys={new Set([window])}
          onSelectionChange={(k) => setWindow(String([...k][0]) as "24h" | "7d")}
          aria-label="Latency window"
        >
          <ToggleButton key="24h" id="24h">24 hours</ToggleButton>
          <ToggleButtonGroup.Separator />
          <ToggleButton key="7d" id="7d">7 days</ToggleButton>
        </ToggleButtonGroup>
      </div>

      {loading && !data && (
        <p className="py-6 text-center text-xs text-muted">Loading latency statistics…</p>
      )}

      {error && (
        <div className="p-3 text-xs text-danger">{error}</div>
      )}

      {data && (
        <div className="space-y-3 p-3">
          <p className="text-xs text-muted leading-relaxed">
            {data.notice || "Sampled from successful requests with reply audio only; failed, cancelled and unmeasured requests are excluded."}
          </p>

          {(!data.paths || data.paths.length === 0) ? (
            <p className="py-2 text-xs text-muted">No successful requests with reply audio in this window.</p>
          ) : (
            <div className="space-y-3">
              {data.paths.map((p) => (
                <div key={p.path} className="space-y-2 rounded-lg border border-separator/60 bg-surface-secondary/40 p-2.5 text-xs">
                  <div className="flex items-center justify-between font-semibold">
                    <span>{p.label || p.path}</span>
                    <span className="text-muted font-normal">
                      {p.response_count} response{p.response_count === 1 ? "" : "s"} ({p.attempts} attempt{p.attempts === 1 ? "" : "s"})
                    </span>
                  </div>

                  {p.response && p.response.n > 0 && (
                    <div className="flex items-center justify-between rounded bg-surface px-2 py-1">
                      <span className="font-medium text-foreground">Response</span>
                      <span className="font-mono tabular-nums text-muted">
                        {p.response.n} sample{p.response.n === 1 ? "" : "s"} · p50 {formatLatencyMS(p.response.p50)} · p95 {formatLatencyMS(p.response.p95)}
                      </span>
                    </div>
                  )}

                  {p.stage_list && p.stage_list.length > 0 && (
                    <div className="overflow-x-auto">
                      <table className="w-full text-left text-xs">
                        <thead>
                          <tr className="border-b border-separator/60 text-muted">
                            <th className="py-1 pr-2 font-normal">Stage</th>
                            <th className="py-1 px-2 font-normal text-right">Samples</th>
                            <th className="py-1 px-2 font-normal text-right">p50</th>
                            <th className="py-1 pl-2 font-normal text-right">p95</th>
                          </tr>
                        </thead>
                        <tbody className="divide-y divide-separator/40 font-mono">
                          {p.stage_list.map((s) => (
                            <tr key={s.name}>
                              <td className="py-1 pr-2 font-sans text-foreground">{s.label}</td>
                              <td className="py-1 px-2 text-right tabular-nums text-muted">{s.n}</td>
                              <td className="py-1 px-2 text-right tabular-nums">{formatLatencyMS(s.p50)}</td>
                              <td className="py-1 pl-2 text-right tabular-nums">{formatLatencyMS(s.p95)}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  )}

                  {(p.first_chunk.n > 0 || p.agent_token.n > 0) && (
                    <div className="space-y-1 border-t border-separator/40 pt-1 text-muted">
                      {p.first_chunk.n > 0 && (
                        <div className="flex items-center justify-between">
                          <span>First reply chunk</span>
                          <span className="font-mono tabular-nums">
                            {p.first_chunk.n} sample{p.first_chunk.n === 1 ? "" : "s"} · p50 {formatLatencyMS(p.first_chunk.p50)} · p95 {formatLatencyMS(p.first_chunk.p95)}
                          </span>
                        </div>
                      )}
                      {p.agent_token.n > 0 && (
                        <div className="flex items-center justify-between">
                          <span>Agent first token</span>
                          <span className="font-mono tabular-nums">
                            {p.agent_token.n} sample{p.agent_token.n === 1 ? "" : "s"} · p50 {formatLatencyMS(p.agent_token.p50)} · p95 {formatLatencyMS(p.agent_token.p95)}
                          </span>
                        </div>
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </Section>
  );
}

