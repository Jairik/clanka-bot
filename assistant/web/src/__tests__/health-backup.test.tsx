// The Health page's backup is an authenticated same-origin POST of the one
// SQLite database. These drive the real button so the route and method cannot
// drift from the control the owner sees.
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
  Wakeword: [],
  Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
  Modules: [],
  Spotify: [],
  Routing: { Name: "Semantic routing", Status: "not-configured", StatusLabel: "not configured", Detail: "Semantic routing is not configured in this deployment." },
  FalseWakes: null,
  DiagnosticsURL: "/api/ui/diagnostics",
};

const dbBlob = () =>
  new Response(new Uint8Array([0x53, 0x51, 0x4c, 0x69, 0x74, 0x65, 0x20, 0x66, 0x6f, 0x72, 0x6d, 0x61, 0x74, 0x20, 0x33, 0x00]), {
    status: 200,
    headers: { "Content-Type": "application/octet-stream", "Content-Disposition": 'attachment; filename=assistant-20261116T120000Z.db' },
  });

/** Install a fetch that records the calls it gets and answers both routes. */
const serve = () => {
  const seen: Array<{ url: string; method?: string; init?: RequestInit }> = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push({ url: String(input), method: init?.method, init });
    const path = new URL(String(input), "http://localhost").pathname;
    return path === "/api/ui/backup" ? dbBlob() : json(health);
  }) as typeof globalThis.fetch;
  return seen;
};

beforeEach(() => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:backup") });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});

it("downloads the backup route as a POST from the Health page", async () => {
  const seen = serve();
  render(<HealthPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Download database backup/i }));

  const backupCalls = seen.filter((call) => call.url === "/api/ui/backup");
  expect(backupCalls).toHaveLength(1);
  expect(backupCalls[0].method).toBe("POST");
  expect(backupCalls[0].init?.credentials).toBe("same-origin");
});

it("warns that the backup contains the secrets table", async () => {
  serve();
  render(<HealthPage />);
  expect(await screen.findByText(/contains the secrets table/i)).toBeTruthy();
});
