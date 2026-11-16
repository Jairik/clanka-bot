// The semantic routing row is an in-memory build state, not a service probe.
// These drive the real Health page so the row cannot drift from the view
// contract: a healthy index shows its Tool count, a degraded one is honest.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { HealthPage } from "../pages/Health";
import type { Check, HealthView } from "../types";
import { json, stubFetch } from "./harness";

const health = (routing: Check): HealthView => ({
  Owner: "owner@example.com",
  Page: "health",
  CheckedAt: "now",
  HealthTimeout: "3s",
  Services: [],
  Wakeword: [],
  Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
  Modules: [],
  Spotify: [],
  Routing: routing,
  FalseWakes: null,
  DiagnosticsURL: "/api/ui/diagnostics",
});

const show = async (routing: Check) => {
  stubFetch({ "/api/ui/health": () => json(health(routing)) });
  render(<HealthPage />);
  await userEvent.click(await screen.findByRole("button", { name: /Semantic routing/i }));
};

it("shows the indexed Tool count for a built semantic index", async () => {
  await show({
    Name: "Semantic routing",
    Status: "ok",
    StatusLabel: "ok",
    Detail: "3 Tools indexed; last rebuilt 2026-11-16 12:00:00 UTC",
  });

  expect(await screen.findByText("3 Tools indexed; last rebuilt 2026-11-16 12:00:00 UTC")).toBeTruthy();
  expect(screen.getByText("ok")).toBeTruthy();
});

it("shows a failed rebuild instead of a fake ok", async () => {
  await show({
    Name: "Semantic routing",
    Status: "error",
    StatusLabel: "rebuild failed",
    Detail: "Last Examples rebuild failed: embed service is down.",
  });

  expect(await screen.findByText("rebuild failed")).toBeTruthy();
  expect(screen.getByText("Last Examples rebuild failed: embed service is down.")).toBeTruthy();
});

it("shows not built yet honestly when no Tool is indexed", async () => {
  await show({
    Name: "Semantic routing",
    Status: "error",
    StatusLabel: "not built yet",
    Detail: "No Tools are indexed; semantic routing is unavailable until the index is built.",
  });

  expect(await screen.findByText("not built yet")).toBeTruthy();
  expect(screen.getByText(/No Tools are indexed/)).toBeTruthy();
});
