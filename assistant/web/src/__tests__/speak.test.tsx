// The speak opt-in: the top-bar toggle and the composer's promise.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { SessionProvider } from "../shell";
import { SpeakHint, SpeakProvider, SpeakToggle } from "../speak";
import { emptyActivity, json, session, stubFetch } from "./harness";

function view(ui: React.ReactNode) {
  return render(
    <SessionProvider>
      <SpeakProvider>{ui}</SpeakProvider>
    </SessionProvider>,
  );
}

const routes = (speak: boolean, write?: (body: string) => Promise<Response> | Response) => ({
  "/api/ui/session": () => json(session({ Speak: speak })),
  "/api/ui/activity": () => json(emptyActivity()),
  "/api/ui/speak": (init?: RequestInit) => {
    const body = String(init?.body ?? "");
    return write ? write(body) : json({ Speak: body.includes("value=true") });
  },
});

describe("SpeakToggle", () => {
  it("shows the off state and saves the opt-in", async () => {
    let posted = "";
    stubFetch(routes(false, (body) => ((posted = body), json({ Speak: true }))));
    const user = userEvent.setup();
    view(<SpeakToggle />);

    const toggle = await screen.findByRole("button", { name: "Speak replies on the Pi" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    await user.click(toggle);

    expect(posted).toContain("value=true");
    expect((await screen.findByRole("button", { name: "Speak replies on the Pi" })).getAttribute("aria-pressed")).toBe("true");
  });

  it("puts the toggle back when the server refuses", async () => {
    stubFetch(routes(false, () => new Response("That did not save.", { status: 500 })));
    const user = userEvent.setup();
    view(<SpeakToggle />);

    await user.click(await screen.findByRole("button", { name: "Speak replies on the Pi" }));
    expect((await screen.findByRole("status")).textContent).toContain("That did not save.");
    expect(screen.getByRole("button", { name: "Speak replies on the Pi" }).getAttribute("aria-pressed")).toBe("false");
  });

  it("is hidden when no speaker is wired", async () => {
    let fetched = 0;
    stubFetch({
      "/api/ui/session": () => {
        fetched += 1;
        return json(session({ CanSpeak: false }));
      },
    });
    view(<SpeakToggle />);
    await waitFor(() => expect(fetched).toBe(1));
    expect(screen.queryByRole("button", { name: "Speak replies on the Pi" })).toBeNull();
  });
});

describe("SpeakHint", () => {
  it("says the answer is only shown by default", async () => {
    stubFetch(routes(false));
    view(<SpeakHint />);
    expect(await screen.findByText("Shown only")).toBeTruthy();
  });

  it("says the answer is spoken when the opt-in is on", async () => {
    stubFetch(routes(true));
    view(<SpeakHint />);
    expect(await screen.findByText("Spoken on the Pi")).toBeTruthy();
  });
});
