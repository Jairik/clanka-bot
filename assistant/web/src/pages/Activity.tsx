// Activity: the request history, newest first and grouped by hour. The first
// page is the live ring for an instant view; older pages and searches read the
// assistant's durable history, one bounded page at a time.
import { useEffect, useMemo, useState } from "react";
import { Button, SearchField, ToggleButton, ToggleButtonGroup } from "@heroui/react";
import { ChevronDown, Info, X } from "lucide-react";
import { getJSON, useQueryParam } from "../api";
import { activityQuery, asFilter, bucketActivity, filterActivity, FILTERS, FILTER_LABEL } from "../activity";
import { OutcomeBadge, RequestDetail, fmtClock, isFailed, sourceIcon, useChat } from "../chat";
import { PageFrame } from "../shell";
import type { ActivityView, Request } from "../types";
import { Badge, Empty, Fold, Notice, Section, cn, foldOpen } from "../ui";

type Page = { key: string; list: Request[]; next: string };

export function ActivityPage() {
  const { view, requests, loadError } = useChat();
  const [q, setQ] = useQueryParam("q");
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
          setFeed({ key, list: v.Requests ?? [], next: v.Next });
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
          ? { key, list: [...prev.list, ...(page.Requests ?? [])], next: page.Next }
          : { key, list: page.Requests ?? [], next: page.Next },
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
        <SearchField aria-label="Search activity" value={q} onChange={setQ} className="min-w-0 flex-1 basis-60">
          <SearchField.Group>
            <SearchField.SearchIcon />
            <SearchField.Input placeholder={durable ? "Search all activity" : "Search requests, replies or tools"} />
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

      <div className="space-y-3" aria-busy={searching || loadingOlder}>
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
