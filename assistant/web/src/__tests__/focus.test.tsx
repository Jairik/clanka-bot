// The Focus strip in the Activity header: shows what holds Focus and how many
// announcements are waiting, never any announcement text (#217).
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { FocusStatus, emptyFocus } from "../focus";
import type { FocusView } from "../types";

const view = (over: Partial<FocusView> = {}): FocusView => ({
  ...emptyFocus(),
  ...over,
});

describe("FocusStatus", () => {
  it("shows Listening when nothing holds Focus", () => {
    render(<FocusStatus view={emptyFocus()} />);
    expect(screen.getByRole("status").textContent).toContain("Listening");
  });

  it("names each holder that holds Focus", () => {
    render(<FocusStatus view={view({ ring: true, reply: true, music: false })} />);
    const status = screen.getByRole("status");
    expect(status.textContent).toContain("Ringing");
    expect(status.textContent).toContain("Reply");
    expect(status.textContent).not.toContain("Music");
  });

  it("reports the waiting count, a hold and playback", () => {
    render(
      <FocusStatus view={view({ announcements: { waiting: 2, held: true, playing: true } })} />,
    );
    const status = screen.getByRole("status");
    expect(status.textContent).toContain("2 announcements waiting");
    expect(status.textContent).toContain("held");
    expect(status.textContent).toContain("playing");
  });

  it("never renders announcement text", () => {
    const secret = "a private reminder the Dashboard must never show";
    render(<FocusStatus view={view({ announcements: { waiting: 1, held: false, playing: false } })} />);
    expect(screen.queryByText(secret)).toBeNull();
    expect(screen.getByRole("status").textContent).not.toContain(secret);
  });
});
