// Clean up rendered components and restore device/network fakes between tests.
import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); history.replaceState(null, "", "/"); });
Object.defineProperty(window, "matchMedia", { writable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} }) });
globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
