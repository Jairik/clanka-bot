import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { SettingsPage } from "../pages/Settings";
import type { RoutingExplainView, SettingsView } from "../types";
import { json, stubFetch } from "./harness";

// settingsFixture builds a base SettingsView fixture for tests.
const settingsFixture = (over: Partial<SettingsView> = {}): SettingsView => ({
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
  Live: [],
  Restart: [],
  Voice: "alba",
  CanPreview: false,
  PreviewMaxRunes: 240,
  Wakeword: { Enabled: false, Addr: "", Models: [], Note: "" },
  ...over,
});

// serveSettings stubs the settings and equalizer endpoints with optional route additions.
const serveSettings = (settings: SettingsView, extra: Parameters<typeof stubFetch>[0] = {}) =>
  stubFetch({
    "/api/ui/settings": () => json(settings),
    "/api/ui/equalizer": () =>
      json({
        Profile: { Preset: "Flat", Bands: Array(10).fill(0) },
        Custom: { Preset: "Custom", Bands: Array(10).fill(0) },
        Error: "",
      }),
    ...extra,
  });

it("renders scores and the reason for a sample explain response", async () => {
  const sampleExplain: RoutingExplainView = {
    path: "agent_path",
    tool: "",
    args: "",
    top: [
      { tool: "lights.off", score: 0.62 },
      { tool: "clock.now", score: 0.45 },
      { tool: "timer.set", score: 0.31 },
    ],
    margin: 0.17,
    extract: "none",
    fallthrough_reason: "below_threshold",
    chain_split: false,
    focus_owner: "",
    decision: "agent fallback",
  };

  serveSettings(settingsFixture(), {
    "/api/ui/routing/explain": () => json(sampleExplain),
  });

  render(<SettingsPage />);

  // Wait for settings page to load.
  expect(await screen.findByText(/Explain routing/i)).toBeTruthy();
  expect(screen.getByText(/Explanations are side-effect free\./i)).toBeTruthy();

  const input = screen.getByLabelText("Phrase to explain");
  await userEvent.type(input, "turn off the kitchen lights");

  const button = screen.getByRole("button", { name: "Explain" });
  await userEvent.click(button);

  // Assert decision line is rendered.
  expect(await screen.findByTestId("decision-line")).toBeTruthy();
  expect(screen.getByTestId("decision-line").textContent).toContain("agent fallback");

  // Assert reason is rendered.
  expect(screen.getByText("below_threshold")).toBeTruthy();

  // Assert top tools table with scores is rendered.
  expect(screen.getByText("lights.off")).toBeTruthy();
  expect(screen.getByText("0.620")).toBeTruthy();
  expect(screen.getByText("clock.now")).toBeTruthy();
  expect(screen.getByText("0.450")).toBeTruthy();
  expect(screen.getByText("timer.set")).toBeTruthy();
  expect(screen.getByText("0.310")).toBeTruthy();
});
