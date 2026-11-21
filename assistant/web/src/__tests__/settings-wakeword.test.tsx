import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { SettingsPage } from "../pages/Settings";
import type { Setting, SettingsView } from "../types";
import { json, stubFetch } from "./harness";

const makeSetting = (model: string, val: string): Setting => ({
  Key: `wakeword.threshold.${model}`,
  Anchor: `wakeword-threshold-${model}`,
  Label: "Threshold",
  Description: `Detection threshold for ${model}; higher is stricter. Applies to the next detection.`,
  FormKind: "decimal",
  Value: val,
  Display: val,
  Edit: val,
  Bool: false,
  Source: "config",
  SourceLabel: "config.yaml",
  LowerLabel: "config.yaml",
  Override: false,
  Restart: false,
  SaveAction: `/settings/values/wakeword.threshold.${model}`,
  ResetAction: `/settings/values/wakeword.threshold.${model}/reset`,
  Target: "",
  PreviewAction: "",
  PreviewKind: "",
});

const view = (over: Partial<SettingsView> = {}): SettingsView => ({
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
  Wakeword: {
    Enabled: true,
    Addr: "wakeword:10400",
    Note: "The model list and address are connection settings.",
    Models: [
      {
        Name: "clanker",
        Threshold: "0.60",
        Command: false,
        Enabled: true,
        Setting: makeSetting("clanker", "0.6"),
      },
      {
        Name: "stop",
        Threshold: "0.70",
        Command: true,
        Enabled: true,
        Setting: null, // Read-only fallback
      },
    ],
  },
  ...over,
});

const serve = (settings: SettingsView) =>
  stubFetch({
    "/api/ui/settings": () => json(settings),
    "/api/ui/equalizer": () =>
      json({ Profile: { Preset: "Flat", Bands: Array(10).fill(0) }, Custom: { Preset: "Custom", Bands: Array(10).fill(0) }, Error: "" }),
  });

it("renders wake word settings with address, live form, and read-only fallback", async () => {
  serve(view());
  render(<SettingsPage />);

  // Address
  expect(await screen.findByText("wakeword:10400")).toBeTruthy();

  // Model names and stop badge
  expect(screen.getByText("clanker")).toBeTruthy();
  expect(screen.getByText("stop")).toBeTruthy();
  expect(screen.getByText("stop · reply only")).toBeTruthy();

  // clanker has a live Setting row with an input field
  const input = screen.getByLabelText("Threshold") as HTMLInputElement;
  expect(input).toBeTruthy();
  expect(input.value).toBe("0.6");

  // stop has no live Setting, so it renders the read-only threshold
  expect(screen.getByText("threshold 0.70")).toBeTruthy();
});
