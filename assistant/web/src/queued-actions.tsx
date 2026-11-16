// Deferred actions are the pipeline's expiring, cancellable requests. The queue
// answers {items, limit}, so every surface shows the server's real cap (#297).
import { useCallback, useEffect, useState } from "react";
import { Button } from "@heroui/react";
import { HttpError, getJSON, postForm } from "./api";
import { useVisiblePolling } from "./visibility";

export type DeferredItem = { id: number; event: string; text: string; expires: string };
export type DeferredView = { items: DeferredItem[]; limit: number };

/** No `deferred` live delta exists yet, so a visibility-gated poll is the fallback (#281). */
export const DEFERRED_POLL_MS = 10_000;
const TICK_MS = 30_000;

/** Round up so an action never reads "0 min" while it is still waiting. */
export function expiresIn(expires: string, now: number): string {
  const minutes = Math.max(0, Math.ceil((new Date(expires).getTime() - now) / 60_000));
  if (minutes === 0) return "expired";
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest ? `${hours} h ${rest} min` : `${hours} h`;
}

function useNow() {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    // A local clock is not a request to the Pi, so it keeps ticking while the
    // tab is open; only the fetch is gated on visibility.
    const id = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(id);
  }, []);
  return now;
}

export function useDeferredQueue() {
  const [view, setView] = useState<DeferredView>({ items: [], limit: 0 });
  const [error, setError] = useState("");
  const [pending, setPending] = useState<number | null>(null);
  const refresh = useCallback(
    () =>
      getJSON<DeferredView>("/api/deferred")
        .then((v) => {
          setView(v);
          setError("");
        })
        .catch((e) => {
          // A deployment without the queue answers 404. That is an empty
          // queue, not a failure — the smoke harness does not mount it.
          if (e instanceof HttpError && e.status === 404) {
            setView({ items: [], limit: 0 });
            setError("");
          } else {
            setError(String(e));
          }
        }),
    [],
  );
  useEffect(() => {
    void refresh();
  }, [refresh]);
  useVisiblePolling(refresh, DEFERRED_POLL_MS);

  const cancel = useCallback(
    async (id: number) => {
      setPending(id);
      try {
        const result = await postForm("/api/deferred", { id: String(id) });
        if (!result.ok) throw new Error(result.text || `Cancel failed (${result.status})`);
        await refresh();
      } catch (e) {
        setError(String(e));
      } finally {
        setPending(null);
      }
    },
    [refresh],
  );
  return { ...view, error, pending, cancel };
}

/** The shared read-only row list, used by the Home card, Alerts and Settings. */
export function DeferredRows({
  items, error, pending, cancel,
}: {
  items: DeferredItem[];
  error: string;
  pending: number | null;
  cancel: (id: number) => Promise<void>;
}) {
  const now = useNow();
  return (
    <div className="space-y-2">
      {error && <p role="alert" className="px-3.5 text-xs text-danger">{error}</p>}
      {items.length === 0 ? (
        <p className="px-3.5 py-2 text-sm text-muted">Nothing is queued.</p>
      ) : (
        <ul className="divide-y divide-separator">
          {items.map((item) => (
            <li key={item.id} className="flex min-h-14 flex-wrap items-center gap-3 px-3.5 py-2.5">
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm">{item.text}</div>
                <div className="truncate text-[11px] text-muted">
                  after {item.event} · <span className="tabular-nums">expires in {expiresIn(item.expires, now)}</span>
                </div>
              </div>
              <Button size="sm" variant="secondary" isPending={pending === item.id} onPress={() => void cancel(item.id)}>
                Cancel
              </Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
