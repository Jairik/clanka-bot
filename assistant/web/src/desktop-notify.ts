// Desktop reminders: a browser notification for a due Reminder, on the same
// Dashboard surface. The server decides whether the channel is allowed
// (`AlertsView.DesktopNotify`), reusing `notify.reminders` and Quiet hours, so
// this hook never invents a second preference. It only schedules the client
// timer; a cancelled Reminder re-arms the set on the next view update.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

export type DesktopReminder = {
  KindKey: string;
  Label: string;
  NextISO: string;
  CancelAction: string;
};

export type DesktopNotifyState = {
  supported: boolean;
  permission: NotificationPermission | "unsupported";
  enabled: boolean;
  blockedReason: string;
  request: () => Promise<void>;
};

const MAX_DELAY_MS = 24 * 60 * 60 * 1000;

const supported = () => typeof window !== "undefined" && "Notification" in window;

/**
 * Schedule one browser Notification per active Reminder. The timer is cleared
 * when the view changes, the page unmounts, or the channel is disabled.
 */
export function useDesktopNotifications(
  view: { DesktopNotify?: boolean; Alerts?: DesktopReminder[] | null } | null,
): DesktopNotifyState {
  const [permission, setPermission] = useState<NotificationPermission | "unsupported">(() =>
    supported() ? Notification.permission : "unsupported",
  );
  const timers = useRef<number[]>([]);

  const enabled = supported() && permission === "granted" && view?.DesktopNotify === true;
  const alerts = view?.Alerts;

  useEffect(() => {
    for (const id of timers.current) window.clearTimeout(id);
    timers.current = [];
    if (!enabled || !alerts?.length) return;
    // A browser timeout is a 32-bit signed delay (about 24.8 days); this hook
    // re-arms in bounded chunks so a Reminder weeks away is not dropped and a
    // long-lived tab still fires on time.
    const schedule = (alert: DesktopReminder) => {
      const at = Date.parse(alert.NextISO);
      if (!Number.isFinite(at) || at <= Date.now()) return;
      const tick = () => {
        const remaining = at - Date.now();
        if (remaining <= 0) {
          try {
            new Notification("Reminder", {
              body: alert.Label || "A reminder is due.",
              tag: alert.CancelAction,
            });
          } catch {
            // A browser that exposes Notification but refuses the constructor
            // (for example a permission change mid-session) must not break the
            // page; the next render shows the blocked state instead.
          }
          return;
        }
        const id = window.setTimeout(tick, Math.min(remaining, MAX_DELAY_MS));
        timers.current.push(id);
      };
      tick();
    };
    for (const alert of alerts) {
      if (alert.KindKey === "reminder") schedule(alert);
    }
    return () => {
      for (const id of timers.current) window.clearTimeout(id);
      timers.current = [];
    };
  }, [enabled, alerts]);

  const request = useCallback(async () => {
    if (!supported()) return;
    try {
      const result = await Notification.requestPermission();
      setPermission(result);
    } catch {
      setPermission(Notification.permission);
    }
  }, []);

  const blockedReason = useMemo(() => {
    if (!supported()) return "This browser has no desktop notifications.";
    if (permission === "denied") return "Desktop notifications are blocked in the browser settings.";
    if (view?.DesktopNotify !== true) return "Notifications are off in Settings, or Quiet hours is active.";
    const reminders = (alerts ?? []).filter((a) => a.KindKey === "reminder");
    if (!reminders.length) return "No Reminder is scheduled.";
    return "";
  }, [permission, view?.DesktopNotify, alerts]);

  return { supported: supported(), permission, enabled, blockedReason, request };
}
