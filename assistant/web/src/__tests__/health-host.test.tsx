import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { HealthPage } from "../pages/Health";
import type { Check, HealthView } from "../types";
import { json, stubFetch } from "./harness";

const health = (host: Check[]): HealthView => ({
  Owner: "owner@example.com",
  Page: "health",
  CheckedAt: "2026-11-16 12:00:00 UTC",
  HealthTimeout: "3s",
  Services: [],
  Host: host,
  Wakeword: [],
  Location: { Name: "location", Status: "ok", StatusLabel: "ok", Detail: "Bristol" },
  Modules: [],
  Spotify: [],
  Routing: { Name: "Semantic routing", Status: "ok", StatusLabel: "ok", Detail: "12 Tools indexed" },
  FalseWakes: null,
  DiagnosticsURL: "/api/ui/diagnostics",
});

it("shows the Pi (host) and data section with all healthy rows", async () => {
  const hostChecks: Check[] = [
    { Name: "CPU temperature", Status: "ok", StatusLabel: "ok", Detail: "59.3 °C" },
    { Name: "Load average", Status: "ok", StatusLabel: "ok", Detail: "0.70, 0.83, 0.76 (1/5/15 min)" },
    { Name: "Memory available", Status: "ok", StatusLabel: "ok", Detail: "5.4 GB available" },
    { Name: "Uptime", Status: "ok", StatusLabel: "ok", Detail: "3 days, 4 hours" },
    { Name: "Database size", Status: "ok", StatusLabel: "ok", Detail: "1.2 MB (WAL: 0 B)" },
    { Name: "Activity history", Status: "ok", StatusLabel: "ok", Detail: "420 rows, oldest 2026-11-01 12:00:00 UTC" },
    { Name: "Free space", Status: "ok", StatusLabel: "ok", Detail: "28.5 GB free (58%)" },
  ];

  stubFetch({ "/api/ui/health": () => json(health(hostChecks)) });
  render(<HealthPage />);

  // Open the section
  const sectionButton = await screen.findByRole("button", { name: /Pi \(host\) and data/i });
  expect(sectionButton).toBeTruthy();
  await userEvent.click(sectionButton);

  expect(screen.getByText("CPU temperature")).toBeTruthy();
  expect(screen.getByText("59.3 °C")).toBeTruthy();
  expect(screen.getByText("Load average")).toBeTruthy();
  expect(screen.getByText("0.70, 0.83, 0.76 (1/5/15 min)")).toBeTruthy();
  expect(screen.getByText("Memory available")).toBeTruthy();
  expect(screen.getByText("5.4 GB available")).toBeTruthy();
  expect(screen.getByText("Uptime")).toBeTruthy();
  expect(screen.getByText("3 days, 4 hours")).toBeTruthy();
  expect(screen.getByText("Database size")).toBeTruthy();
  expect(screen.getByText("1.2 MB (WAL: 0 B)")).toBeTruthy();
  expect(screen.getByText("Activity history")).toBeTruthy();
  expect(screen.getByText("420 rows, oldest 2026-11-01 12:00:00 UTC")).toBeTruthy();
  expect(screen.getByText("Free space")).toBeTruthy();
  expect(screen.getByText("28.5 GB free (58%)")).toBeTruthy();
});

it("marks unavailable rows distinctly", async () => {
  const hostChecks: Check[] = [
    { Name: "CPU temperature", Status: "unavailable", StatusLabel: "unavailable", Detail: "Thermal sensor unavailable." },
    { Name: "Free space", Status: "unavailable", StatusLabel: "unavailable", Detail: "Data volume free space unavailable." },
  ];

  stubFetch({ "/api/ui/health": () => json(health(hostChecks)) });
  render(<HealthPage />);

  const sectionButton = await screen.findByRole("button", { name: /Pi \(host\) and data/i });
  await userEvent.click(sectionButton);

  expect(screen.getByText("CPU temperature")).toBeTruthy();
  expect(screen.getByText("Thermal sensor unavailable.")).toBeTruthy();
  expect(screen.getByText("Free space")).toBeTruthy();
  expect(screen.getByText("Data volume free space unavailable.")).toBeTruthy();
  expect(screen.getAllByText("unavailable").length).toBeGreaterThanOrEqual(2);
});

it("opens automatically and shows warning badges when thresholds are crossed", async () => {
  const hostChecks: Check[] = [
    { Name: "CPU temperature", Status: "error", StatusLabel: "hot", Detail: "85.2 °C" },
    { Name: "Memory available", Status: "error", StatusLabel: "low memory", Detail: "150 MB available" },
    { Name: "Free space", Status: "error", StatusLabel: "full", Detail: "100 MB free (1%)" },
  ];

  stubFetch({ "/api/ui/health": () => json(health(hostChecks)) });
  render(<HealthPage />);

  // The section is defaultOpen when bad > 0
  expect(await screen.findByText("CPU temperature")).toBeTruthy();
  expect(screen.getByText("hot")).toBeTruthy();
  expect(screen.getByText("85.2 °C")).toBeTruthy();
  expect(screen.getByText("low memory")).toBeTruthy();
  expect(screen.getByText("150 MB available")).toBeTruthy();
  expect(screen.getByText("full")).toBeTruthy();
  expect(screen.getByText("100 MB free (1%)")).toBeTruthy();
});
