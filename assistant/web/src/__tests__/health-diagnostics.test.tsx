// The Health page's download is an authenticated GET of the one support
// bundle. These drive the real button and checkbox so the route (and the
// requests flag) cannot drift from the control the owner sees.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { HealthPage } from "../pages/Health";
import type { HealthView } from "../types";
import { json } from "./harness";

const health: HealthView = {
  Owner: "owner@example.com",
  Page: "health",
  CheckedAt: "now",
  HealthTimeout: "3s",
  Services: [],
  Host: [],
  Wakeword: [],
  Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
  Modules: [],
  Spotify: [],
  Routing: { Name: "Semantic routing", Status: "not-configured", StatusLabel: "not configured", Detail: "Semantic routing is not configured in this deployment." },
  FalseWakes: null,
  DiagnosticsURL: "/api/ui/diagnostics",
};

const bundle = () =>
  new Response(new Uint8Array([123, 125]), {
    status: 200,
    headers: { "Content-Type": "application/json", "Content-Disposition": 'attachment; filename="clanka-diagnostics-20261116T120000Z.json"' },
  });

/** Install a fetch that records the URLs it gets and answers both reads. */
const serve = () => {
  const seen: Array<{ url: string; init?: RequestInit }> = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push({ url: String(input), init });
    const path = new URL(String(input), "http://localhost").pathname;
    return path === "/api/ui/diagnostics" ? bundle() : json(health);
  }) as typeof globalThis.fetch;
  return seen;
};

beforeEach(() => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:diagnostics") });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});

it("downloads the diagnostics route from the Health page", async () => {
  const seen = serve();
  render(<HealthPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Download diagnostics/i }));

  expect(seen.filter((call) => call.url === "/api/ui/diagnostics")).toHaveLength(1);
  expect(seen.at(-1)?.init?.credentials).toBe("same-origin");
});

it("adds the requests flag only when the private-text checkbox is on", async () => {
  const seen = serve();
  render(<HealthPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Download diagnostics/i }));
  expect(seen.at(-1)?.url).toBe("/api/ui/diagnostics");

  await userEvent.click(screen.getByRole("checkbox", { name: /last 50 requests/i }));
  await userEvent.click(screen.getByRole("button", { name: /Download diagnostics/i }));
  expect(seen.at(-1)?.url).toBe("/api/ui/diagnostics?requests=1");
});
