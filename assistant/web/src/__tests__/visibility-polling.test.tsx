// Fallback pollers must cost the Pi nothing while the tab is hidden (#281).
// The helper is exercised directly for the three visibility invariants, then
// through the two real consumers (the shell health dot and the Settings queue)
// so the endpoints they hit stay covered too.
import { act, render, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { SettingsPage } from "../pages/Settings";
import { Shell } from "../shell";
import { useVisiblePolling } from "../visibility";
import type { HealthView, Setting, SettingsView } from "../types";
import { json, stubFetch } from "./harness";

const setVisibility = (state: "visible" | "hidden") => {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
};

const changeVisibility = (state: "visible" | "hidden") => {
  setVisibility(state);
  document.dispatchEvent(new Event("visibilitychange"));
};

// Flush the promise chain behind a getJSON stub without running real timers.
const settle = async () => {
  await act(async () => {});
};

describe("useVisiblePolling", () => {
  it("does not poll while the tab is hidden", () => {
    vi.useFakeTimers();
    setVisibility("hidden");
    const load = vi.fn();
    renderHook(() => useVisiblePolling(load, 3_000));

    act(() => vi.advanceTimersByTime(90_000));
    expect(load).not.toHaveBeenCalled();
  });

  it("resumes with exactly one immediate call when the tab becomes visible", () => {
    vi.useFakeTimers();
    setVisibility("hidden");
    const load = vi.fn();
    renderHook(() => useVisiblePolling(load, 3_000));
    act(() => vi.advanceTimersByTime(90_000));
    expect(load).not.toHaveBeenCalled();

    act(() => changeVisibility("visible"));
    expect(load).toHaveBeenCalledTimes(1);

    // The interval resumes from here; it does not stack on the catch-up call.
    act(() => vi.advanceTimersByTime(3_000));
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("keeps one poller after repeated visibility changes", () => {
    vi.useFakeTimers();
    setVisibility("visible");
    const load = vi.fn();
    renderHook(() => useVisiblePolling(load, 3_000));

    for (let i = 0; i < 5; i += 1) {
      act(() => changeVisibility("hidden"));
      act(() => changeVisibility("visible"));
    }
    // One catch-up call per visible transition, not one per listener registered.
    expect(load).toHaveBeenCalledTimes(5);

    load.mockClear();
    act(() => vi.advanceTimersByTime(3_000));
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("does not poll while disabled", () => {
    vi.useFakeTimers();
    setVisibility("visible");
    const load = vi.fn();
    renderHook(() => useVisiblePolling(load, 3_000, false));

    act(() => vi.advanceTimersByTime(90_000));
    act(() => changeVisibility("hidden"));
    act(() => changeVisibility("visible"));
    expect(load).not.toHaveBeenCalled();
  });
});

describe("shell health poll", () => {
  it("makes no health request while hidden and one on return", async () => {
    vi.useFakeTimers();
    setVisibility("hidden");
    const health = vi.fn(() =>
      json({
        Owner: "owner@example.com",
        Page: "health",
        CheckedAt: "now",
        HealthTimeout: "3s",
        Services: [],
        Modules: [],
        Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
        Wakeword: [],
        Spotify: [],
      } satisfies HealthView),
    );
    stubFetch({ "/api/ui/health": health });
    render(<Shell><div /></Shell>);
    await settle();
    expect(health).toHaveBeenCalledTimes(1); // the initial snapshot on mount

    act(() => vi.advanceTimersByTime(90_000));
    expect(health).toHaveBeenCalledTimes(1);

    await act(async () => {
      changeVisibility("visible");
    });
    await settle();
    expect(health).toHaveBeenCalledTimes(2);
  });
});

const liveSetting = (): Setting => ({
  Key: "chained.commands",
  Anchor: "chained.commands",
  Label: "Chained commands",
  Description: "",
  FormKind: "text",
  Value: "",
  Edit: "edit",
  Display: "",
  Bool: false,
  Source: "default",
  SourceLabel: "Default",
  LowerLabel: "chained commands",
  Restart: false,
  Override: false,
  SaveAction: "/settings/values/chained.commands",
  ResetAction: "",
  Target: "",
  PreviewAction: "",
  PreviewKind: "",
});

const settingsView = (): SettingsView => ({
  Owner: "owner@example.com",
  Page: "settings",
  Flash: "",
  Error: "",
  HasSettings: true,
  Tools: [],
  Ignored: [],
  Agent: [],
  ToolWording: [],
  Quiet: [],
  Live: [liveSetting()], // mounts the deferred-queue section
  Restart: [],
  Voice: "alba",
  CanPreview: false,
  PreviewMaxRunes: 240,
  Wakeword: { Enabled: false, Addr: "", Models: [], Note: "" },
});

describe("Settings deferred queue poll", () => {
  it("makes no deferred request while hidden and one on return", async () => {
    vi.useFakeTimers();
    setVisibility("hidden");
    const deferred = vi.fn(() => json({ items: [], limit: 8 }));
    stubFetch({
      "/api/ui/settings": () => json(settingsView()),
      "/api/ui/equalizer": () =>
        json({ Profile: { Preset: "Flat", Bands: Array(10).fill(0) }, Custom: { Preset: "Custom", Bands: Array(10).fill(0) }, Error: "" }),
      "/api/deferred": deferred,
    });
    render(<SettingsPage />);
    await settle();
    expect(deferred).toHaveBeenCalledTimes(1); // the initial queue on mount

    act(() => vi.advanceTimersByTime(90_000));
    expect(deferred).toHaveBeenCalledTimes(1);

    await act(async () => {
      changeVisibility("visible");
    });
    await settle();
    expect(deferred).toHaveBeenCalledTimes(2);
  });
});
