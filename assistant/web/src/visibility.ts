// Shared fallback-polling helper. Every poll in the app must cost the Pi
// nothing while the tab is hidden (#281); the stream closes on hide, so these
// fallbacks are the only requests a backgrounded Dashboard would still make.
import { useEffect } from "react";

/**
 * Run `load` every `everyMs`, but only while the tab is visible, and run it once
 * immediately when the tab becomes visible again. One interval and one listener
 * per mount, so repeated visibility changes cannot spawn duplicate pollers.
 */
export function useVisiblePolling(load: () => void, everyMs: number, enabled = true) {
  useEffect(() => {
    if (!enabled) return;
    const poll = () => {
      if (document.visibilityState === "visible") load();
    };
    const id = setInterval(poll, everyMs);
    document.addEventListener("visibilitychange", poll);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", poll);
    };
  }, [load, everyMs, enabled]);
}
