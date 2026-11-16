// The Refresh button must show its in-flight state and order concurrent checks
// so a slow earlier reply cannot overwrite a fresher view. See issue #250.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { HealthPage } from "../pages/Health";
import { usePageData } from "../api";
import type { HealthView } from "../types";
import { json, stubFetch } from "./harness";

const health = (over: Partial<HealthView> = {}): HealthView => ({
  Owner: "owner@example.com",
  Page: "health",
  CheckedAt: "now",
  HealthTimeout: "3s",
  Services: [],
  Wakeword: [],
  Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
  Modules: [],
  Spotify: [],
  Routing: { Name: "Semantic routing", Status: "ok", StatusLabel: "ok", Detail: "" },
  FalseWakes: null,
  DiagnosticsURL: "/api/ui/diagnostics",
  ...over,
});

// A plain-button harness over the real hook, so two clicks both fire (the hook
// is what guards ordering, not the UI's disabled state).
function Reloader() {
  const { data, refreshing, reload } = usePageData("health");
  return (
    <div>
      <button onClick={() => void reload()}>reload</button>
      <span data-testid="refreshing">{refreshing ? "yes" : "no"}</span>
      <span data-testid="checked">{data?.CheckedAt ?? ""}</span>
    </div>
  );
}

describe("usePageData refreshing state", () => {
  it("sets refreshing until the fetch settles", async () => {
    let resolve!: (r: Response) => void;
    stubFetch({ "/api/ui/health": () => new Promise<Response>((r) => (resolve = r)) });

    render(<Reloader />);
    expect((await screen.findByTestId("refreshing")).textContent).toBe("yes");

    resolve(json(health({ CheckedAt: "1" })));
    await screen.findByTestId("checked");
    expect(screen.getByTestId("refreshing").textContent).toBe("no");
  });

  it("keeps the newer view when two refreshes race out of order", async () => {
    const pending: Array<(r: Response) => void> = [];
    stubFetch({ "/api/ui/health": () => new Promise<Response>((r) => pending.push(r)) });

    render(<Reloader />);
    const btn = await screen.findByRole("button", { name: "reload" });

    // First load.
    pending.shift()!(json(health({ CheckedAt: "0" })));
    await screen.findByText("0", { selector: '[data-testid="checked"]' });

    // Two rapid refreshes; both fetches are in flight before either resolves.
    await userEvent.click(btn);
    await userEvent.click(btn);
    expect(pending).toHaveLength(2);

    const older = pending[0];
    const newer = pending[1];
    // Resolve the older check LAST; the guard must keep the newer view.
    newer(json(health({ CheckedAt: "2" })));
    older(json(health({ CheckedAt: "1" })));

    await waitFor(() => expect(screen.getByTestId("checked").textContent).toBe("2"));
    expect(screen.getByTestId("refreshing").textContent).toBe("no");
  });
});

describe("Health Refresh button feedback", () => {
  it("disables and shows Checking… while a refresh runs, then reports the result", async () => {
    let resolve!: (r: Response) => void;
    stubFetch({ "/api/ui/health": () => new Promise<Response>((r) => (resolve = r)) });

    render(<HealthPage />);
    const refresh = (await screen.findByRole("button", { name: /Refresh/i })) as HTMLButtonElement;
    // First load: the button is pending (disabled) with no result yet.
    expect(refresh.disabled).toBe(true);
    expect(screen.queryByText(/Checked/)).toBeNull();

    resolve(json(health({ CheckedAt: "1" })));
    await screen.findByText(/Checked 1/);
    expect(refresh.disabled).toBe(false);

    // A manual refresh shows the in-flight label and clears it on settle.
    await userEvent.click(refresh);
    expect(screen.getByText("Checking…")).toBeTruthy();
    expect((screen.getByRole("button", { name: /Refresh/i }) as HTMLButtonElement).disabled).toBe(true);
  });
});
