// The listening control: wake-word pause and timed snooze from the top bar.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { ListeningControl, SessionProvider } from "../shell";
import type { Listening } from "../types";
import { emptyActivity, json, session, stubFetch } from "./harness";

function view(ui: React.ReactNode) {
  return render(<SessionProvider>{ui}</SessionProvider>);
}

const listeningView = (over: Partial<Listening> = {}): Listening => ({
  Available: true,
  Listening: true,
  Until: "",
  Error: "",
  ...over,
});

const routes = (
  sessionOver: Record<string, unknown> = {},
  listening: Listening = listeningView(),
  write?: (body: string) => Promise<Response> | Response,
) => ({
  "/api/ui/session": () => json(session({ Wakeword: true, ...sessionOver })),
  "/api/ui/activity": () => json(emptyActivity()),
  "/api/ui/listening": (init?: RequestInit) => {
    if (init?.method === "POST") {
      const body = String(init.body ?? "");
      return write ? write(body) : json(listening);
    }
    return json(listening);
  },
});

describe("ListeningControl", () => {
  it("is hidden when Wakeword is not wired in session", async () => {
    let fetched = 0;
    stubFetch({
      "/api/ui/session": () => {
        fetched += 1;
        return json(session({ Wakeword: false }));
      },
      "/api/ui/activity": () => json(emptyActivity()),
    });
    view(<ListeningControl />);
    await waitFor(() => expect(fetched).toBe(1));
    expect(screen.queryByRole("button", { name: /listening/i })).toBeNull();
  });

  it("shows active listening state and opens popover options", async () => {
    stubFetch(routes());
    const user = userEvent.setup();
    view(<ListeningControl />);

    const button = await screen.findByRole("button", { name: "Pause wake-word listening" });
    expect(button.getAttribute("aria-pressed")).toBe("false");
    expect(screen.queryByText("paused")).toBeNull();

    await user.click(button);
    expect(await screen.findByRole("heading", { name: "Listening" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Pause indefinitely" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "15m" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "30m" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "60m" })).toBeTruthy();
  });

  it("pauses indefinitely when clicking Pause indefinitely", async () => {
    let posted = "";
    stubFetch(
      routes({}, listeningView(), (body) => {
        posted = body;
        return json(listeningView({ Listening: false }));
      }),
    );
    const user = userEvent.setup();
    view(<ListeningControl />);

    await user.click(await screen.findByRole("button", { name: "Pause wake-word listening" }));
    await user.click(await screen.findByRole("button", { name: "Pause indefinitely" }));

    expect(posted).toContain("listening=false");
    expect((await screen.findAllByText("paused")).length).toBeGreaterThan(0);
  });

  it("pauses for timed duration when clicking 15m", async () => {
    let posted = "";
    const futureTime = new Date(Date.now() + 15 * 60 * 1000).toISOString();
    stubFetch(
      routes({}, listeningView(), (body) => {
        posted = body;
        return json(listeningView({ Listening: true, Until: futureTime }));
      }),
    );
    const user = userEvent.setup();
    view(<ListeningControl />);

    await user.click(await screen.findByRole("button", { name: "Pause wake-word listening" }));
    await user.click(await screen.findByRole("button", { name: "15m" }));

    expect(posted).toContain("duration=15m");
    expect((await screen.findAllByText("paused")).length).toBeGreaterThan(0);
  });

  it("shows paused badge and allows resume when paused indefinitely", async () => {
    let posted = "";
    stubFetch(
      routes({}, listeningView({ Listening: false }), (body) => {
        posted = body;
        return json(listeningView({ Listening: true, Until: "" }));
      }),
    );
    const user = userEvent.setup();
    view(<ListeningControl />);

    expect((await screen.findAllByText("paused")).length).toBeGreaterThan(0);
    const button = await screen.findByRole("button", { name: "Resume wake-word listening" });
    expect(button.getAttribute("aria-pressed")).toBe("true");

    await user.click(button);
    const resumeBtn = await screen.findByRole("button", { name: "Resume listening" });
    await user.click(resumeBtn);

    expect(posted).toContain("listening=true");
    await waitFor(() => {
      expect(screen.queryByText("paused")).toBeNull();
    });
  });

  it("shows paused badge and until time when paused until future", async () => {
    const futureDate = new Date(Date.now() + 30 * 60 * 1000);
    stubFetch(routes({}, listeningView({ Listening: true, Until: futureDate.toISOString() })));
    const user = userEvent.setup();
    view(<ListeningControl />);

    expect((await screen.findAllByText("paused")).length).toBeGreaterThan(0);
    await user.click(await screen.findByRole("button", { name: "Resume wake-word listening" }));
    expect(await screen.findByText(/Paused until/)).toBeTruthy();
  });

  it("displays server error when update is refused", async () => {
    stubFetch(
      routes({}, listeningView(), () => new Response("Database write failed.", { status: 500 })),
    );
    const user = userEvent.setup();
    view(<ListeningControl />);

    await user.click(await screen.findByRole("button", { name: "Pause wake-word listening" }));
    await user.click(await screen.findByRole("button", { name: "Pause indefinitely" }));

    expect((await screen.findByRole("status")).textContent).toContain("Database write failed.");
  });
});
