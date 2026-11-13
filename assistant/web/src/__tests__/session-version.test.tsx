// The UI and the assistant ship separately: a view-shape mismatch must be a
// visible refusal, never a half-rendered page.
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Providers, json, session, stubFetch } from "./harness";

describe("session API version", () => {
  it("refuses a payload whose response header names another view shape", async () => {
    // An open tab survives an assistant upgrade; the body may still parse, but
    // the header says the shape moved. The session is the first request, so the
    // mismatch page appears before any page renders.
    stubFetch({
      "/api/ui/session": () =>
        new Response(JSON.stringify(session()), {
          status: 200,
          headers: { "Content-Type": "application/json", "X-Dashboard-API-Version": "999" },
        }),
    });
    render(
      <Providers>
        <p>app content</p>
      </Providers>,
    );
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("API v999");
    expect(screen.queryByText("app content")).toBeNull();
  });

  it("refuses to render the app for another view shape", async () => {
    stubFetch({ "/api/ui/session": () => json(session({ APIVersion: 999 })) });
    render(
      <Providers>
        <p>app content</p>
      </Providers>,
    );
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("API v999");
    expect(alert.textContent).toContain("do not match");
    expect(screen.queryByText("app content")).toBeNull();
  });
});
