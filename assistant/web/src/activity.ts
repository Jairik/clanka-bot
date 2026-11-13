// Pure Activity page logic: the filter/search predicate and the hour buckets.
// Kept out of the page component so the list rules can be tested without a DOM.
import type { Request } from "./types";

export const FILTERS = ["all", "voice", "typed", "mic", "problems"] as const;
export type ActivityFilter = (typeof FILTERS)[number];
export const FILTER_LABEL: Record<ActivityFilter, string> = {
  all: "All",
  voice: "Voice",
  typed: "Typed",
  mic: "Mic",
  problems: "Problems",
};

/** Typed on a keyboard: the Dashboard box or a debug request. */
export const isTypedSource = (s: string) => s === "dashboard" || s === "debug";
export const isProblem = (r: Request) => r.Outcome === "failed" || r.Outcome === "cancelled" || !!r.Error;

export const asFilter = (f: string): ActivityFilter => ((FILTERS as readonly string[]).includes(f) ? (f as ActivityFilter) : "all");

/** The three input paths: voice at the Pi, typed on the page, mic in the browser. */
const matches = (r: Request, filter: ActivityFilter) =>
  filter === "voice"
    ? r.Source === "voice"
    : filter === "typed"
      ? isTypedSource(r.Source)
      : filter === "mic"
        ? r.Source === "dashboard_mic"
        : filter === "problems"
          ? isProblem(r)
          : true;

/** Case-insensitive match over everything the row shows. */
const haystack = (r: Request) => `${r.Transcript} ${r.Reply} ${r.Tool} ${r.PathLabel} ${r.Error}`.toLowerCase();

/** Order is preserved: the caller passes newest-first. */
export function filterActivity(requests: Request[], filter: string, query: string): Request[] {
  const f = asFilter(filter);
  const needle = query.trim().toLowerCase();
  return requests.filter((r) => matches(r, f)).filter((r) => !needle || haystack(r).includes(needle));
}

/**
 * The paged history read. Defaults are omitted so the plain route stays the
 * first page and an unchanged URL is cacheable by the service worker's
 * network-first rule.
 */
export function activityQuery({ filter, q, before }: { filter: string; q: string; before?: string }): string {
  const params = new URLSearchParams();
  const f = asFilter(filter);
  const text = q.trim();
  if (f !== "all") params.set("filter", f);
  if (text) params.set("q", text);
  if (before) params.set("before", before);
  const search = params.toString();
  return `/api/ui/activity${search ? `?${search}` : ""}`;
}

/**
 * Apply one live delta to a newest-first list. Deduping by request id keeps a
 * POST response and the stream from showing the same answer twice.
 */
export function mergeActivity(requests: Request[], entry: Request, limit: number): Request[] {
  return [entry, ...requests.filter((r) => r.ID !== entry.ID)].slice(0, Math.max(1, limit));
}

export type ActivityBucket = { key: string; title: string; list: Request[] };

/**
 * Group by hour, keeping the input order. The key carries the day as well as
 * the hour: the ring can span more than a week, so "Mon 2 PM" from two weeks
 * would otherwise share one bucket and one duplicate React key.
 */
export function bucketActivity(requests: Request[], now = new Date()): ActivityBucket[] {
  const buckets = new Map<string, ActivityBucket>();
  const today = now.toDateString();
  for (const r of requests) {
    const d = new Date(r.TimeISO);
    const key = `${d.toDateString()}|${d.getHours()}`;
    const hour = d.toLocaleString([], { weekday: "short", hour: "numeric" });
    const title = d.toDateString() === today ? hour : `${hour} · ${d.toLocaleDateString([], { month: "short", day: "numeric" })}`;
    const entry = buckets.get(key) ?? { key, title, list: [] };
    entry.list.push(r);
    buckets.set(key, entry);
  }
  return [...buckets.values()];
}
