// Per-row pending state: every Phrase row posts to the same URL, so the spinner
// must be keyed on the Tool, not the URL. Saving one row must not light up the
// Save or Reset of any other row (#199).
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { PhrasebookPage } from "../pages/Phrasebook";
import type { PhrasebookView } from "../types";
import { json, stubFetch } from "./harness";

const base = (): PhrasebookView => ({
  Owner: "owner@example.com",
  Page: "phrasebook",
  Flash: "",
  Error: "",
  HasService: true,
  HasGenerator: true,
  ActivePersona: "Ada",
  Selected: "Ada",
  Personas: [{ Name: "Ada", Active: true, Count: 2 }],
  Slots: [
    { Tool: "lights.power", Group: "lights", Description: "Turn a light on or off.", Default: "Done.", Phrase: "Done.", Source: "default", Overridden: true },
    { Tool: "timer.set", Group: "timer", Description: "Set a timer.", Default: "Set.", Phrase: "Set.", Source: "default", Overridden: true },
  ],
  Total: 2,
  Overridden: 2,
  Generated: 0,
  Manual: 0,
});

it("shows the spinner only on the row that was saved", async () => {
  // Hold the save response so the pending state is observable.
  let resolveSave: (r: Response) => void = () => {};
  stubFetch({
    "/api/ui/phrasebook": () => json(base()),
    "/phrasebook/Ada": () => new Promise<Response>((resolve) => {
      resolveSave = resolve;
    }),
  });
  const user = userEvent.setup();
  render(<PhrasebookPage />);
  await screen.findByText("lights.power");

  // Both overrides are edited so each row shows its own Save button.
  const lightsField = screen.getByLabelText("lights.power phrase") as HTMLInputElement;
  const timerField = screen.getByLabelText("timer.set phrase") as HTMLInputElement;
  await user.clear(lightsField);
  await user.type(lightsField, "Lights on.");
  await user.clear(timerField);
  await user.type(timerField, "Timer's ticking.");

  const rows = screen.getAllByRole("listitem");
  const lightsRow = rows.find((r) => within(r).queryByText("lights.power"))!;
  const timerRow = rows.find((r) => within(r).queryByText("timer.set"))!;
  const lightsSave = within(lightsRow).getByRole("button", { name: "Save" });
  const timerSave = within(timerRow).getByRole("button", { name: "Save" });
  const timerReset = within(timerRow).getByRole("button", { name: "Reset" });

  await user.click(lightsSave);

  // Only the row that was touched is pending; a sibling row's Save and Reset
  // stay interactive. HeroUI surfaces the pending state as data-pending + an
  // aria-disabled button rather than the native disabled flag.
  const isPending = (b: HTMLElement) => b.getAttribute("data-pending") === "true";
  await waitFor(() => expect(isPending(lightsSave)).toBe(true));
  expect(isPending(timerSave)).toBe(false);
  expect(isPending(timerReset)).toBe(false);

  await act(async () => {
    resolveSave(json(base()));
  });
  await waitFor(() => expect(isPending(lightsSave)).toBe(false));
});
