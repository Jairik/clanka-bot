// Which route chunks a first visit downloads: the page either side of the
// current one, and only when the link can afford it. The old prefetchAll()
// imported all sixteen.
import { afterEach, describe, expect, it, vi } from "vitest";

// Two spy modules stand in for the real pages, so a fetch here is observable.
const loaded = vi.hoisted(() => ({ activity: false, settings: false }));

vi.mock("../pages/Activity", () => {
  loaded.activity = true;
  return { ActivityPage: () => null };
});

vi.mock("../pages/Settings", () => {
  loaded.settings = true;
  return { SettingsPage: () => null };
});

import { cheapLink, prefetch, prefetchNear, railOrder } from "../routes";
import { PAGES } from "../shell";

const ids = (load: ReturnType<typeof vi.fn>) => load.mock.calls.map(([id]) => id);

/** Pretend the browser reports a Network Information value. */
const withConnection = (value: unknown) =>
  Object.defineProperty(navigator, "connection", { configurable: true, value });

afterEach(() => {
  delete (navigator as unknown as Record<string, unknown>).connection;
});

describe("prefetchNear", () => {
  it("warms the pages either side of the current one", () => {
    const load = vi.fn();
    prefetchNear("sounds", load);
    expect(ids(load)).toEqual(["health", "voices"]);
  });

  it("warms only the neighbour that exists at either end of the rail", () => {
    const atHome = vi.fn();
    prefetchNear("home", atHome);
    expect(ids(atHome)).toEqual(["activity"]);

    const atSettings = vi.fn();
    prefetchNear("settings", atSettings);
    expect(ids(atSettings)).toEqual(["hermes"]);
  });

  it("does nothing on a saveData connection", () => {
    withConnection({ saveData: true, effectiveType: "4g" });
    const load = vi.fn();
    prefetchNear("sounds", load);
    expect(load).not.toHaveBeenCalled();
  });

  it("does nothing on a 2G connection", () => {
    for (const effectiveType of ["slow-2g", "2g"]) {
      withConnection({ saveData: false, effectiveType });
      const load = vi.fn();
      prefetchNear("sounds", load);
      expect(load).not.toHaveBeenCalled();
    }
  });

  it("ignores a route the rail does not have", () => {
    const load = vi.fn();
    prefetchNear(undefined, load);
    prefetchNear(null, load);
    expect(load).not.toHaveBeenCalled();
  });
});

describe("cheapLink", () => {
  it("is true when the browser reports nothing", () => {
    expect(cheapLink(undefined)).toBe(true);
    expect(cheapLink({})).toBe(true);
  });

  it("follows saveData and the effective type", () => {
    expect(cheapLink({ saveData: true, effectiveType: "4g" })).toBe(false);
    expect(cheapLink({ effectiveType: "3g" })).toBe(true);
    expect(cheapLink({ effectiveType: "4g" })).toBe(true);
    expect(cheapLink({ effectiveType: "slow-2g" })).toBe(false);
    expect(cheapLink({ effectiveType: "2g" })).toBe(false);
  });
});

describe("route chunks", () => {
  it("keeps the rail order in step with the shell's pages", () => {
    expect(railOrder).toEqual(PAGES.map((page) => page.id));
  });

  it("loads nothing until a prefetch asks for a page", () => {
    expect(loaded.activity).toBe(false);
    expect(loaded.settings).toBe(false);
  });

  it("fetches the current page's neighbour on idle, not every page", async () => {
    prefetchNear("home");
    await vi.waitFor(() => expect(loaded.activity).toBe(true));
    expect(loaded.settings).toBe(false);
  });

  it("fetches one page after hover, without warming the rest", async () => {
    prefetch("settings");
    await vi.waitFor(() => expect(loaded.settings).toBe(true));
  });
});
