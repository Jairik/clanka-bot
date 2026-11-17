// The false-wake measurement is a Health row driven by the generated view
// contract. These drive the real page so the "since restart" label, the
// per-model counts and the zero-denominator "unknown" rate cannot drift.
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { HealthPage } from "../pages/Health";
import type { HealthView } from "../types";
import { json, stubFetch } from "./harness";

const health = (falseWakes: HealthView["FalseWakes"]): HealthView => ({
  Owner: "owner@example.com",
  Page: "health",
  CheckedAt: "now",
  HealthTimeout: "3s",
  Services: [],
  Host: [],
  Wakeword: [],
  FalseWakes: falseWakes,
  Location: { Name: "pi", Status: "ok", StatusLabel: "OK", Detail: "" },
  Modules: [],
  Spotify: [],
  Routing: { Name: "Semantic routing", Status: "not-configured", StatusLabel: "not configured", Detail: "" },
  DiagnosticsURL: "/api/ui/diagnostics",
});

const show = async (falseWakes: HealthView["FalseWakes"]) => {
  stubFetch({ "/api/ui/health": () => json(health(falseWakes)) });
  render(<HealthPage />);
  await screen.findByText("Checked now");
};

it("renders the false-wake counts and rate per wake model since restart", async () => {
  await show([
    { Model: "okay_nabu", Wakes: 4, NoSpeech: 1, Unroutable: 1, Rate: "50%" },
    { Model: "clanker", Wakes: 0, NoSpeech: 0, Unroutable: 0, Rate: "unknown" },
  ]);

  expect(screen.getByText("False wakes (since restart)")).toBeTruthy();
  expect(screen.getByText("false-wake rate 50%")).toBeTruthy();
  expect(screen.getByText(/1 no speech · 1 unroutable · 4 wakes/)).toBeTruthy();
  // Zero denominator reads as unknown, never as a fabricated 0%.
  expect(screen.getByText("false-wake rate unknown")).toBeTruthy();
});

it("omits the row when voice input is off", async () => {
  await show(null);
  expect(screen.queryByText("False wakes (since restart)")).toBeNull();
});
