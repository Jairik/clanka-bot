import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useDesktopNotifications } from "../desktop-notify";

function Probe({ view }: { view: Parameters<typeof useDesktopNotifications>[0] }) {
  const notify = useDesktopNotifications(view);
  return (
    <div>
      <span data-testid="enabled">{String(notify.enabled)}</span>
      <span data-testid="status">{notify.permission}</span>
      <span data-testid="reason">{notify.blockedReason}</span>
      <button onClick={() => void notify.request()}>enable</button>
    </div>
  );
}

class FakeNotification {
  static permission: NotificationPermission = "granted";
  static requestPermission = vi.fn(async () => FakeNotification.permission);
  static instances: { title: string; body?: string; tag?: string }[] = [];
  constructor(
    public title: string,
    public options?: { body?: string; tag?: string },
  ) {
    FakeNotification.instances.push({ title, body: options?.body, tag: options?.tag });
  }
}

const reminder = (dueInMs: number) => ({
  KindKey: "reminder",
  Label: "check the oven",
  NextISO: new Date(Date.now() + dueInMs).toISOString(),
  CancelAction: "/alerts/reminder/1/cancel",
});

describe("useDesktopNotifications", () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: new Date("2026-11-14T10:00:00Z") });
    vi.stubGlobal("Notification", FakeNotification);
    FakeNotification.permission = "granted";
    FakeNotification.instances = [];
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("fires one notification when a Reminder is due and the server allows it", () => {
    render(<Probe view={{ DesktopNotify: true, Alerts: [reminder(1000)] }} />);
    expect(screen.getByTestId("enabled").textContent).toBe("true");
    act(() => vi.advanceTimersByTime(1500));
    expect(FakeNotification.instances).toEqual([
      { title: "Reminder", body: "check the oven", tag: "/alerts/reminder/1/cancel" },
    ]);
  });

  it("stays silent when the server says the channel is off", () => {
    render(<Probe view={{ DesktopNotify: false, Alerts: [reminder(1000)] }} />);
    act(() => vi.advanceTimersByTime(1500));
    expect(FakeNotification.instances).toHaveLength(0);
    expect(screen.getByTestId("reason").textContent).toContain("Notifications are off");
  });

  it("clears a pending notification when the reminder disappears", () => {
    const view = { DesktopNotify: true, Alerts: [reminder(1000)] };
    const { rerender } = render(<Probe view={view} />);
    rerender(<Probe view={{ DesktopNotify: true, Alerts: [] }} />);
    act(() => vi.advanceTimersByTime(1500));
    expect(FakeNotification.instances).toHaveLength(0);
  });

  it("asks the browser for permission", async () => {
    FakeNotification.permission = "default";
    render(<Probe view={{ DesktopNotify: true, Alerts: [reminder(1000)] }} />);
    expect(screen.getByTestId("status").textContent).toBe("default");
    FakeNotification.permission = "granted";
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "enable" }));
    });
    expect(FakeNotification.requestPermission).toHaveBeenCalled();
    expect(screen.getByTestId("status").textContent).toBe("granted");
  });
});
