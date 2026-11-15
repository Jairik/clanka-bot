// A stored override that no longer validates is reported in Settings with the
// reset that removes it, so recovery never needs SQLite surgery.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { SettingsPage } from "../pages/Settings";
import type { SettingsView } from "../types";
import { json, stubFetch } from "./harness";

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
  Wakeword: { Enabled: false, Addr: "", Models: [], Note: "" },
  ...over,
});

/** The page also mounts the Equalizer, which loads its own view. */
const serve = (settings: SettingsView, extra: Parameters<typeof stubFetch>[0] = {}) =>
  stubFetch({
    "/api/ui/settings": () => json(settings),
    "/api/ui/equalizer": () =>
      json({ Profile: { Preset: "Flat", Bands: Array(10).fill(0) }, Custom: { Preset: "Custom", Bands: Array(10).fill(0) }, Error: "" }),
    ...extra,
  });

it("reports an ignored override and resets it from the page", async () => {
  const ignored = {
    Key: "http.addr",
    Label: "HTTP address",
    Reason: "must not be empty",
    ResetAction: "/settings/values/http.addr/reset",
  };
  let resetBody = "";
  serve(view({ Ignored: [ignored] }), {
    [ignored.ResetAction]: (init) => {
      resetBody = String(init?.body ?? "");
      return json(view({ Flash: "Reset. The config.yaml or code-default value applies again." }));
    },
  });
  render(<SettingsPage />);

  // The key and the validator's reason are visible without exposing the value.
  expect(await screen.findByText("http.addr")).toBeTruthy();
  expect(screen.getByText("must not be empty")).toBeTruthy();
  expect(screen.getByText("ignored")).toBeTruthy();

  await userEvent.click(screen.getByRole("button", { name: "Reset HTTP address" }));
  expect(resetBody).toBe("");
  expect(await screen.findByText(/Reset\. The config\.yaml or code-default value applies again\./)).toBeTruthy();
  expect(screen.queryByText("ignored")).toBeNull();
  expect(screen.queryByRole("button", { name: "Reset HTTP address" })).toBeNull();
});
