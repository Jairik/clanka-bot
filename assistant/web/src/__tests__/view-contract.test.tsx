// The Lights page reads two views through usePageData (the Bluetooth room
// controllers and the Monster cloud lights), and the Equalizer once mirrored
// its view by hand: a version mismatch must show the shared message on any
// read, and exactly one alert for one mismatched endpoint.
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Equalizer } from "../pages/Equalizer";
import { LightsPage } from "../pages/Lights";
import { json, stubFetch } from "./harness";

const mismatched = () =>
  new Response(JSON.stringify({}), {
    status: 200,
    headers: { "Content-Type": "application/json", "X-Dashboard-API-Version": "999" },
  });

const lights = () => json({ enabled: false, status: "", devices: [] });
const monster = () => json({ configured: true, status: "", devices: [] });

describe("the equalizer page", () => {
  it("shows the shared API version mismatch message", async () => {
    stubFetch({ "/api/ui/equalizer": () => mismatched() });
    render(<Equalizer />);
    // The Equalizer's Section is collapsed by default, so its notice is hidden
    // until found; the Lights page renders its notices in the open page frame.
    const alert = await screen.findByRole("alert", { hidden: true });
    expect(alert.textContent).toContain("API v999");
    expect(alert.textContent).toContain("Deploy the dashboard-web and assistant images from the same commit.");
  });
});

describe.each([
  ["the Bluetooth view", "/api/ui/lights"],
  ["the Monster view", "/api/ui/monster"],
] as const)("the lights page when %s is from another build", (_, path) => {
  it("shows one shared API version mismatch alert", async () => {
    stubFetch({
      "/api/ui/lights": path === "/api/ui/lights" ? mismatched : lights,
      "/api/ui/monster": path === "/api/ui/monster" ? mismatched : monster,
    });
    render(<LightsPage />);

    const alerts = await screen.findAllByRole("alert");
    expect(alerts).toHaveLength(1);
    expect(alerts[0].textContent).toContain("API v999");
    expect(alerts[0].textContent).toContain("Deploy the dashboard-web and assistant images from the same commit.");
  });
});
