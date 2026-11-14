// One live stream for the whole app (#94). The provider opens a single
// Server-Sent Events connection, stops the polling fallbacks while it is open,
// and bumps a generation on every open so each view reloads once after a drop:
// an event missed while disconnected is recovered by that full resync.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";

export const LIVE_NAMES = ["activity", "volume", "alerts", "health"] as const;
export type LiveName = (typeof LIVE_NAMES)[number];

export const isLiveName = (value: string): value is LiveName => (LIVE_NAMES as readonly string[]).includes(value);

/** Reconnect delay: 1 s doubling to a 30 s ceiling. */
export const liveBackoff = (attempt: number) => Math.min(1000 * 2 ** attempt, 30_000);

/** A stream that stayed up this long proves the endpoint works, so the backoff resets. */
export const liveStableAfter = 10_000;

/** The server pings every 15 s; this long without any frame means the stream is dead. */
export const liveStaleAfter = 40_000;

/** How often the watchdog looks at the last frame's age. */
export const liveWatchEvery = 5_000;

/** The deltas kept for a resync; one snapshot request never takes longer than this many. */
export const liveResyncLog = 32;

type Listener = (data: unknown) => void;

/**
 * Merge the deltas that arrived while a snapshot request was in flight over
 * the snapshot that answers it (#149). A delta the server produced after
 * reading the snapshot is newer, and the sampler has already advanced its diff
 * key, so a plain assignment would lose it until the next real change.
 */
export type ResyncMerge<T> = (snapshot: T, deltas: unknown[]) => T;

/**
 * The merge for a name whose delta is a whole view (health, alerts, volume):
 * the newest delta is the current state.
 */
export const latestDelta = <T,>(snapshot: T, deltas: unknown[]): T =>
  deltas.length ? (deltas[deltas.length - 1] as T) : snapshot;

type Live = {
  /** The stream is open; polling is paused. */
  live: boolean;
  /** Increments on every open, so a consumer resyncs after a reconnect. */
  generation: number;
  subscribe: (name: LiveName, listener: Listener) => () => void;
  /**
   * Load a snapshot over HTTP and re-apply any delta for `name` that arrived
   * while the request was in flight. `name` is null for a view with no stream.
   */
  resync: <T>(name: LiveName | null, load: () => Promise<T>, merge: ResyncMerge<T>) => Promise<T>;
};

const LiveCtx = createContext<Live>({
  live: false,
  generation: 0,
  subscribe: () => () => {},
  resync: (_name, load) => load(),
});
export const useLive = () => useContext(LiveCtx);

export function LiveProvider({ children }: { children: ReactNode }) {
  const [live, setLive] = useState(false);
  const [generation, setGeneration] = useState(0);
  const listeners = useRef(new Map<LiveName, Set<Listener>>());
  // Deltas are numbered in arrival order and kept briefly: a snapshot request
  // stamps the counter when it starts and replays what arrived after the mark.
  const seq = useRef(0);
  const received = useRef(new Map<LiveName, { seq: number; data: unknown }[]>());

  const subscribe = useCallback((name: LiveName, listener: Listener) => {
    const set = listeners.current.get(name) ?? new Set<Listener>();
    set.add(listener);
    listeners.current.set(name, set);
    return () => {
      set.delete(listener);
      if (!set.size) listeners.current.delete(name);
    };
  }, []);

  const emit = useCallback((name: LiveName, data: unknown) => {
    const log = received.current.get(name) ?? [];
    log.push({ seq: (seq.current += 1), data });
    if (log.length > liveResyncLog) log.shift();
    received.current.set(name, log);
    listeners.current.get(name)?.forEach((listener) => listener(data));
  }, []);

  const resync = useCallback(
    async <T,>(name: LiveName | null, load: () => Promise<T>, merge: ResyncMerge<T>): Promise<T> => {
      // The mark comes before the request: a delta that arrived earlier is
      // already in the snapshot, and replaying it would move the view back.
      const mark = seq.current;
      const snapshot = await load();
      if (!name) return snapshot;
      const deltas = (received.current.get(name) ?? []).filter((e) => e.seq > mark).map((e) => e.data);
      return deltas.length ? merge(snapshot, deltas) : snapshot;
    },
    [],
  );

  useEffect(() => {
    if (typeof EventSource === "undefined") return;
    let source: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let watchdog: ReturnType<typeof setInterval> | undefined;
    let attempt = 0;
    let stopped = false;
    let hidden = document.visibilityState === "hidden";
    let openedAt = 0;
    let lastFrame = 0;

    /** Close the current stream and keep its backoff state. */
    const close = () => {
      if (watchdog) clearInterval(watchdog);
      watchdog = undefined;
      // Reset the backoff only after a connection that actually stayed up: an
      // endpoint that accepts and immediately drops must keep doubling, or
      // every tab reconnects once a second and resyncs every consumer.
      if (openedAt && Date.now() - openedAt >= liveStableAfter) attempt = 0;
      openedAt = 0;
      setLive(false);
      source?.close();
      source = null;
    };

    const reconnect = () => {
      if (stopped || hidden) return;
      timer = setTimeout(connect, liveBackoff(attempt));
      attempt += 1;
    };

    /** Drop a stream that failed or went silent and schedule the next one. */
    const drop = () => {
      if (!source) return;
      close();
      reconnect();
    };

    const connect = () => {
      if (stopped || hidden) return;
      const opened: EventSource = new EventSource("/api/ui/events");
      source = opened;
      opened.onopen = () => {
        if (source !== opened) return;
        openedAt = Date.now();
        lastFrame = openedAt;
        setLive(true);
        // Every open is a resync: the stream carries deltas only, so a view
        // that changed while the stream was down has to be reloaded.
        setGeneration((g) => g + 1);
      };
      opened.onerror = () => {
        if (source !== opened) return;
        drop();
      };
      // The server names its keepalive "ping", so an idle stream still proves
      // it is alive. Without a frame for liveStaleAfter the connection is dead
      // (a proxy or a suspended Pi) even though EventSource raised no error.
      opened.addEventListener("ping", () => {
        if (source === opened) lastFrame = Date.now();
      });
      for (const name of LIVE_NAMES) {
        opened.addEventListener(name, (event) => {
          if (source !== opened) return;
          lastFrame = Date.now();
          let data: unknown;
          try {
            data = JSON.parse((event as MessageEvent<string>).data);
          } catch {
            return;
          }
          emit(name, data);
        });
      }
      watchdog = setInterval(() => {
        if (source === opened && Date.now() - lastFrame >= liveStaleAfter) drop();
      }, liveWatchEvery);
    };

    // A hidden tab must not hold the stream open: the Pi stops its samplers
    // with the last stream, and a background tab should cost nothing. Coming
    // back reopens it, which is also the resync for anything missed meanwhile.
    const onVisibility = () => {
      hidden = document.visibilityState === "hidden";
      if (hidden) {
        clearTimeout(timer);
        close();
        return;
      }
      if (!source && !stopped) {
        attempt = 0;
        connect();
      }
    };

    document.addEventListener("visibilitychange", onVisibility);
    if (!hidden) connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
      close();
    };
  }, [emit]);

  const value = useMemo(() => ({ live, generation, subscribe, resync }), [live, generation, subscribe, resync]);
  return <LiveCtx.Provider value={value}>{children}</LiveCtx.Provider>;
}
