// Data access. Reads are JSON under /api/ui/; writes are form-encoded POSTs to
// the action URLs the server put in each view, and answer with the refreshed
// view (Error set and a 4xx/5xx status when the write was refused).
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { isLiveName, latestDelta, useLive } from "./events";
import { API_VERSION } from "./api-version";
import type { DataPage, PageViews, WriteResult } from "./types";

export class HttpError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

/**
 * The assistant reports the view shape it serves in every response. A page
 * built against another shape refuses the payload instead of rendering fields
 * that moved or disappeared; the session view alone is not enough once a tab
 * stays open across an assistant upgrade.
 */
export class APIVersionMismatch extends Error {
  constructor(public served: number) {
    super(
      `This Dashboard build expects API v${API_VERSION}; the assistant at this address serves API v${served}. Deploy the dashboard-web and assistant images from the same commit.`,
    );
  }
}

export async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" }, signal, credentials: "same-origin" });
  if (!res.ok) throw new HttpError(res.status, (await res.text()).trim() || res.statusText);
  const served = Number(res.headers.get("X-Dashboard-API-Version") ?? "");
  if (Number.isFinite(served) && served > 0 && served !== API_VERSION) throw new APIVersionMismatch(served);
  return res.json() as Promise<T>;
}

/**
 * POST a write. Same-origin fetches carry Sec-Fetch-Site, which the Dashboard's
 * CSRF guard accepts. A refused write still returns the view, so the caller
 * gets both the error text and the preserved state.
 */
export async function postForm<T>(action: string, body?: Record<string, string> | FormData | Blob, init?: RequestInit): Promise<WriteResult<T>> {
  const payload = body instanceof FormData || body instanceof Blob ? body : new URLSearchParams(body ?? {});
  // A take uploads as one raw WAV body, not multipart: the server bounds and
  // validates exactly the bytes the browser produced.
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body instanceof Blob) headers["Content-Type"] = "audio/wav";
  const res = await fetch(action, {
    method: "POST",
    body: payload,
    headers,
    credentials: "same-origin",
    ...init,
  });
  const type = res.headers.get("Content-Type") ?? "";
  if (type.includes("application/json")) {
    return { ok: res.ok, status: res.status, view: (await res.json()) as T, text: "" };
  }
  return { ok: res.ok, status: res.status, view: null, text: (await res.text()).trim() };
}

// ---------- page data ----------

type Load<T> = {
  data: T | null;
  error: string;
  loading: boolean;
  reload: () => Promise<void>;
  /** Replace the view with a write's response. */
  set: (v: T) => void;
};

/**
 * Loads one page view and optionally polls it. Extra query params (?flash= from
 * the Spotify redirect) are forwarded once on the first load.
 */
export function usePageData<P extends DataPage>(page: P, opts: { poll?: number; query?: string } = {}): Load<PageViews[P]> {
  const [data, setData] = useState<PageViews[P] | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const first = useRef(true);
  const query = opts.query;
  const { live, generation, subscribe, resync } = useLive();

  const reload = useCallback(async () => {
    const q = first.current && query ? `?${query}` : "";
    first.current = false;
    try {
      // Health and Alerts arrive as a whole view, so a delta that lands while
      // the snapshot is in flight is the newer state (#149).
      const v = await resync(
        isLiveName(page) ? page : null,
        () => getJSON<PageViews[P]>(`/api/ui/${page}${q}`),
        latestDelta,
      );
      setData(v);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [page, query, resync]);

  useEffect(() => {
    first.current = true;
    setLoading(true);
    void reload();
  }, [reload]);

  useEffect(() => {
    // While the stream is open it carries the page's changes, so the poll is
    // the fallback for a stream that could not be opened or has dropped.
    if (!opts.poll || live) return;
    const id = setInterval(() => {
      if (document.visibilityState === "visible") void reload();
    }, opts.poll);
    return () => clearInterval(id);
  }, [opts.poll, reload, live]);

  // Health and Alerts arrive as a whole view, so a delta replaces the data.
  useEffect(() => {
    if (!isLiveName(page)) return;
    return subscribe(page, (view) => {
      setData(view as PageViews[P]);
      setError("");
      setLoading(false);
    });
  }, [page, subscribe]);

  // Every open resyncs, so a change missed while disconnected is recovered.
  useEffect(() => {
    if (generation > 0) void reload();
  }, [generation, reload]);

  return { data, error, loading, reload, set: setData };
}

// ---------- routing ----------
// Path routing with the History API: /, /activity, /alerts, ... The Go server
// answers every page path with index.html, so reloads and bookmarks work.

const listeners = new Set<() => void>();
const notify = () => listeners.forEach((l) => l());
addEventListener("popstate", notify);

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  if (to === location.pathname + location.search) return;
  if (opts.replace) history.replaceState(null, "", to);
  else history.pushState(null, "", to);
  notify();
}

const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => listeners.delete(l);
};

export function useLocation() {
  const href = useSyncExternalStore(subscribe, () => location.pathname + location.search);
  const url = new URL(href, location.origin);
  return { path: url.pathname, params: url.searchParams };
}

/** One query param as state; replaces history so filters don't pile up Back entries. */
export function useQueryParam(name: string, fallback = ""): [string, (v: string) => void] {
  const { params } = useLocation();
  const value = params.get(name) ?? fallback;
  const set = useCallback(
    (v: string) => {
      const u = new URL(location.href);
      if (v && v !== fallback) u.searchParams.set(name, v);
      else u.searchParams.delete(name);
      navigate(u.pathname + u.search, { replace: true });
    },
    [name, fallback],
  );
  return [value, set];
}
