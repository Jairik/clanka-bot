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

type Listener = (data: unknown) => void;
type Live = {
  /** The stream is open; polling is paused. */
  live: boolean;
  /** Increments on every open, so a consumer resyncs after a reconnect. */
  generation: number;
  subscribe: (name: LiveName, listener: Listener) => () => void;
};

const LiveCtx = createContext<Live>({ live: false, generation: 0, subscribe: () => () => {} });
export const useLive = () => useContext(LiveCtx);

export function LiveProvider({ children }: { children: ReactNode }) {
  const [live, setLive] = useState(false);
  const [generation, setGeneration] = useState(0);
  const listeners = useRef(new Map<LiveName, Set<Listener>>());

  const subscribe = useCallback((name: LiveName, listener: Listener) => {
    const set = listeners.current.get(name) ?? new Set<Listener>();
    set.add(listener);
    listeners.current.set(name, set);
    return () => {
      set.delete(listener);
      if (!set.size) listeners.current.delete(name);
    };
  }, []);

  useEffect(() => {
    if (typeof EventSource === "undefined") return;
    let source: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;
    let stopped = false;

    const connect = () => {
      source = new EventSource("/api/ui/events");
      source.onopen = () => {
        attempt = 0;
        setLive(true);
        // Every open is a resync: the stream carries deltas only, so a view
        // that changed while the stream was down has to be reloaded.
        setGeneration((g) => g + 1);
      };
      source.onerror = () => {
        setLive(false);
        source?.close();
        source = null;
        if (stopped) return;
        timer = setTimeout(connect, liveBackoff(attempt));
        attempt += 1;
      };
      for (const name of LIVE_NAMES) {
        source.addEventListener(name, (event) => {
          let data: unknown;
          try {
            data = JSON.parse((event as MessageEvent<string>).data);
          } catch {
            return;
          }
          listeners.current.get(name)?.forEach((listener) => listener(data));
        });
      }
    };

    connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      source?.close();
    };
  }, []);

  const value = useMemo(() => ({ live, generation, subscribe }), [live, generation, subscribe]);
  return <LiveCtx.Provider value={value}>{children}</LiveCtx.Provider>;
}
