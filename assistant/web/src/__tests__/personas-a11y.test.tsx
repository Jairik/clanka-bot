// Personas: the Select control must not sit inside the disclosure trigger
// button (no nested interactive role), and pressing Select must not toggle the
// row's expand state (#190).
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { PersonasPage } from "../pages/Personas";
import type { PersonasView } from "../types";
import { json, stubFetch } from "./harness";

const view = (over: Partial<PersonasView> = {}): PersonasView => ({
  Owner: "owner@example.com",
  Page: "personas",
  Flash: "",
  Error: "",
  HasService: true,
  Active: null,
  New: {
    Action: "/personas/new",
    Heading: "Create a persona",
    FormID: "persona-new",
    Name: "",
    NameLocked: false,
    Voice: "",
    Personality: "",
    Filler: "",
    Unavailable: "",
    Clarify: "",
    CueWords: "",
    StartPhrases: "",
    EndPhrases: "",
    Submit: "Save",
    Voices: null,
  },
  Rows: [
    {
      Name: "Ada",
      Voice: "en-AU",
      VoiceStatus: "ready",
      Active: true,
      SelectAction: "/personas/ada/select",
      Edit: {
        Action: "/personas/ada", Heading: "", FormID: "persona-ada", Name: "Ada", NameLocked: true,
        Voice: "en-AU", Personality: "", Filler: "", Unavailable: "",
        Clarify: "", CueWords: "", StartPhrases: "", EndPhrases: "", Submit: "Save", Voices: null,
      },
    },
    {
      Name: "Bo",
      Voice: "en-GB",
      VoiceStatus: "ready",
      Active: false,
      SelectAction: "/personas/bo/select",
      Edit: {
        Action: "/personas/bo", Heading: "", FormID: "persona-bo", Name: "Bo", NameLocked: true,
        Voice: "en-GB", Personality: "", Filler: "", Unavailable: "",
        Clarify: "", CueWords: "", StartPhrases: "", EndPhrases: "", Submit: "Save", Voices: null,
      },
    },
  ],
  ...over,
});

/** The disclosure trigger button that wraps a row's header. */
function rowTrigger(rowName: string): HTMLElement {
  const li = screen.getByText(rowName).closest("li")!;
  // The trigger button is the one whose text is the row name; the separate
  // "Use this persona" button is a sibling, not nested inside it.
  return within(li)
    .getAllByRole("button")
    .find((b) => b.textContent?.includes(rowName) && !b.textContent?.includes("Use this persona"))!;
}

it("exposes no interactive control nested inside the disclosure trigger", async () => {
  stubFetch({ "/api/ui/personas": () => json(view()) });
  render(<PersonasPage />);
  await screen.findByText("Bo");

  const trigger = rowTrigger("Bo");
  // The old bug rendered a role=button span inside this trigger; it must be gone.
  expect(within(trigger).queryByRole("button")).toBeNull();

  // The Select action now lives as its own control, in the fold body (which only
  // renders once the row is expanded).
  const user = userEvent.setup();
  await user.click(trigger);
  expect(await screen.findByRole("button", { name: "Use this persona" })).toBeTruthy();
  // And it is a sibling of the trigger, never nested inside it.
  expect(within(rowTrigger("Bo")).queryByRole("button")).toBeNull();
});

it("selecting a persona leaves the row's expand state unchanged", async () => {
  let resolveSelect: (r: Response) => void = () => {};
  stubFetch({
    "/api/ui/personas": () => json(view()),
    "/personas/bo/select": () => new Promise<Response>((resolve) => {
      resolveSelect = resolve;
    }),
  });
  const user = userEvent.setup();
  render(<PersonasPage />);
  await screen.findByText("Bo");

  const trigger = rowTrigger("Bo");
  // Expand the row so the Select control is rendered.
  await user.click(trigger);
  await screen.findByRole("button", { name: "Use this persona" });
  const expandedBefore = trigger.getAttribute("aria-expanded");

  await user.click(screen.getByRole("button", { name: "Use this persona" }));

  // The Select press must not expand or collapse the row.
  expect(trigger.getAttribute("aria-expanded")).toBe(expandedBefore);

  await act(async () => {
    resolveSelect(
      json(
        view({
          Active: { Name: "Bo", Voice: "en-GB", EffectiveVoice: "en-GB", Exact: true, Notes: [] },
          Rows: view().Rows!.map((r) => (r.Name === "Bo" ? { ...r, Active: true } : r)),
        }),
      ),
    );
  });
  // After the write the row is now active, so the Select control is gone.
  await waitFor(() => expect(screen.queryByRole("button", { name: "Use this persona" })).toBeNull());
});
