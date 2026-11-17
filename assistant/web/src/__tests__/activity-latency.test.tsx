// Tests for the Activity page's Latency panel: lazy fetching when opened,
// window selection, stage p50/p95 display, and notice documentation.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { ActivityPage } from "../pages/Activity";
import { emptyActivity, json, Providers, session, stubFetch } from "./harness";
import type { LatencyView } from "../types";

const mockLatency24h: LatencyView = {
  window: "24h",
  records: 12,
  notice: "Sampled from successful requests with reply audio only; failed, cancelled and unmeasured requests are excluded.",
  paths: [
    {
      path: "agent_path",
      label: "Agent path",
      attempts: 15,
      response_count: 12,
      outcomes: { ok: 12, failed: 2, cancelled: 1 },
      response: { n: 12, p50: 2100, p95: 3400 },
      stages: {
        endpointing: { n: 12, p50: 520, p95: 580 },
        routing: { n: 12, p50: 45, p95: 65 },
        tts_first_audio: { n: 12, p50: 130, p95: 180 },
      },
      stage_list: [
        { name: "endpointing", label: "Endpointing", n: 12, p50: 520, p95: 580 },
        { name: "routing", label: "Routing", n: 12, p50: 45, p95: 65 },
        { name: "tts_first_audio", label: "TTS first audio", n: 12, p50: 130, p95: 180 },
      ],
      first_chunk: { n: 12, p50: 1450, p95: 2200 },
      agent_token: { n: 12, p50: 950, p95: 1250 },
    },
  ],
};

const mockLatency7d: LatencyView = {
  window: "7d",
  records: 85,
  notice: "Sampled from successful requests with reply audio only; failed, cancelled and unmeasured requests are excluded.",
  paths: [
    {
      path: "agent_path",
      label: "Agent path",
      attempts: 100,
      response_count: 85,
      outcomes: { ok: 85, failed: 10, cancelled: 5 },
      response: { n: 85, p50: 2200, p95: 3600 },
      stages: {
        endpointing: { n: 85, p50: 530, p95: 590 },
        routing: { n: 85, p50: 50, p95: 70 },
        tts_first_audio: { n: 85, p50: 140, p95: 190 },
      },
      stage_list: [
        { name: "endpointing", label: "Endpointing", n: 85, p50: 530, p95: 590 },
        { name: "routing", label: "Routing", n: 85, p50: 50, p95: 70 },
        { name: "tts_first_audio", label: "TTS first audio", n: 85, p50: 140, p95: 190 },
      ],
      first_chunk: { n: 85, p50: 1500, p95: 2300 },
      agent_token: { n: 85, p50: 980, p95: 1300 },
    },
  ],
};

describe("ActivityPage LatencyPanel", () => {
  it("does not fetch latency on initial page load", async () => {
    let latencyFetched = false;
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json(emptyActivity([])),
      "/api/ui/latency": () => {
        latencyFetched = true;
        return json(mockLatency24h);
      },
    });

    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );

    expect(await screen.findByText("Latency percentiles")).toBeTruthy();
    expect(latencyFetched).toBe(false);
  });

  it("fetches latency when opened and displays percentiles per stage", async () => {
    let requestedWindow = "";
    stubFetch({
      "/api/ui/session": () => json(session()),
      "/api/ui/activity": () => json(emptyActivity([])),
      "/api/ui/latency?window=24h": () => {
        requestedWindow = "24h";
        return json(mockLatency24h);
      },
      "/api/ui/latency?window=7d": () => {
        requestedWindow = "7d";
        return json(mockLatency7d);
      },
    });

    const user = userEvent.setup();
    render(
      <Providers>
        <ActivityPage />
      </Providers>,
    );

    // Wait for the panel trigger to appear and click to open panel
    const trigger = await screen.findByRole("button", { name: /latency percentiles/i });
    fireEvent.click(trigger);

    // Verify it requested the default 24h window
    await waitFor(() => {
      expect(requestedWindow).toBe("24h");
    });

    // Check that stage percentiles are rendered
    expect(await screen.findByText("Endpointing")).toBeTruthy();
    expect(screen.getByText("Routing")).toBeTruthy();
    expect(screen.getByText("TTS first audio")).toBeTruthy();
    expect(screen.getByText("First reply chunk")).toBeTruthy();
    expect(screen.getByText("Agent first token")).toBeTruthy();

    // Check documentation notice
    expect(screen.getByText(/Sampled from successful requests with reply audio only/)).toBeTruthy();

    // Switch window to 7 days
    await user.click(screen.getByRole("radio", { name: "7 days" }));

    await waitFor(() => {
      expect(requestedWindow).toBe("7d");
    });
    expect(await screen.findByText("85 requests in window")).toBeTruthy();
  });
});
