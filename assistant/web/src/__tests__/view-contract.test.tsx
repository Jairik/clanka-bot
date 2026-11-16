// These three pages once mirrored their view types by hand and read with
// getJSON: a version mismatch had to show the shared message here too, not just
// on the pages that already used usePageData.
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Equalizer } from "../pages/Equalizer";
import { LightsPage } from "../pages/Lights";
import { MonsterPage } from "../pages/Monster";
import { stubFetch } from "./harness";

const mismatched = () =>
  new Response(JSON.stringify({}), {
    status: 200,
    headers: { "Content-Type": "application/json", "X-Dashboard-API-Version": "999" },
  });

describe.each([
  ["monster", MonsterPage],
  ["lights", LightsPage],
  ["equalizer", Equalizer],
] as const)("%s page", (page, Page) => {
  it("shows the shared API version mismatch message", async () => {
    stubFetch({ [`/api/ui/${page}`]: () => mismatched() });
    render(<Page />);
    // The Equalizer's Section is collapsed by default, so its notice is hidden
    // until found; the other two pages render theirs in the open page frame.
    const alert = await screen.findByRole("alert", { hidden: true });
    expect(alert.textContent).toContain("API v999");
    expect(alert.textContent).toContain("Deploy the dashboard-web and assistant images from the same commit.");
  });
});
