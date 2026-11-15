// The App's idle prefetch asks for the neighbours of the page on screen. It no
// longer imports every route chunk on a first visit.
import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const prefetchNear = vi.hoisted(() => vi.fn());

vi.mock("../routes", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../routes")>();
  return { ...actual, prefetchNear };
});

import { usePrefetchNear } from "../App";
import type { PageId } from "../shell";

describe("usePrefetchNear", () => {
  it("waits for idle, then asks only for the current page's neighbours", () => {
    vi.useFakeTimers();
    renderHook(() => usePrefetchNear("sounds"));
    expect(prefetchNear).not.toHaveBeenCalled();

    act(() => {
      vi.advanceTimersByTime(1500);
    });
    expect(prefetchNear).toHaveBeenCalledTimes(1);
    expect(prefetchNear).toHaveBeenCalledWith("sounds");
  });

  it("uses requestIdleCallback when the browser has one", () => {
    const idle = vi.fn();
    vi.stubGlobal("requestIdleCallback", idle);
    vi.stubGlobal("cancelIdleCallback", vi.fn());

    renderHook(() => usePrefetchNear("home"));
    expect(idle).toHaveBeenCalledTimes(1);
    expect(prefetchNear).not.toHaveBeenCalled();

    act(() => {
      idle.mock.calls[0][0]();
    });
    expect(prefetchNear).toHaveBeenCalledWith("home");
  });

  it("follows a route change instead of prefetching the rest", () => {
    vi.useFakeTimers();
    const { rerender } = renderHook(({ page }: { page: PageId }) => usePrefetchNear(page), {
      initialProps: { page: "sounds" as PageId },
    });

    act(() => {
      vi.advanceTimersByTime(1500);
    });
    rerender({ page: "settings" });
    act(() => {
      vi.advanceTimersByTime(1500);
    });

    expect(prefetchNear.mock.calls).toEqual([["sounds"], ["settings"]]);
  });

  it("does nothing on a page the rail does not have", () => {
    vi.useFakeTimers();
    renderHook(() => usePrefetchNear(undefined));
    act(() => {
      vi.advanceTimersByTime(1500);
    });
    expect(prefetchNear).not.toHaveBeenCalled();
  });
});
