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
        Voice: "en-AU", Personality: "", Filler: "", Unavailable: "", Submit: "Save", Voices: null,
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
        Voice: "en-GB", Personality: "", Filler: "", Unavailable: "", Submit: "Save", Voices: null,
      },
    },
  ],
  ...over,
});

/** The disclosure trigger button that wraps a row's header. */
function rowTrigger(rowName: string): HTMLElement {
  const li = screen.getByText(rowName, { selector: "li div" }).closest("li")!;
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

it("shows common phrases, export, and named copies on the personas page", async () => {
  let copy: URLSearchParams | undefined;
  stubFetch({
    "/api/ui/personas": () => json(view()),
    "/api/ui/phrasebook": () => json({ HasService: true, Selected: "Ada", Slots: [], Personas: [] }),
    "/api/ui/persona-bundles/copy": (init) => {
      copy = new URLSearchParams(String(init?.body));
      return json(view({ Flash: "Saved a copy as Ada backup." }), 201);
    },
  });
  const user = userEvent.setup();
  render(<PersonasPage />);
  expect(await screen.findByText("Common phrases")).toBeTruthy();
  await user.click(rowTrigger("Ada"));
  expect(screen.getByRole("link", { name: "Export persona and common phrases" }).getAttribute("href"))
    .toBe("/api/ui/persona-bundles/export?name=Ada");
  await user.type(screen.getByRole("textbox", { name: "Copy name for Ada" }), "Ada backup");
  await user.click(screen.getByRole("button", { name: "Save a copy" }));
  expect(await screen.findByText("Saved a copy as Ada backup.")).toBeTruthy();
  expect(copy?.get("source")).toBe("Ada");
  expect(copy?.get("name")).toBe("Ada backup");
});

it("imports a JSON file under the requested name", async () => {
  let posted: URLSearchParams | undefined;
  stubFetch({
    "/api/ui/personas": () => json(view()),
    "/api/ui/phrasebook": () => json({ HasService: true, Slots: [], Personas: [] }),
    "/api/ui/persona-bundles/import": (init) => {
      posted = new URLSearchParams(String(init?.body));
      return json(view({ Flash: "Imported as Ada from Pi." }), 201);
    },
  });
  const user = userEvent.setup();
  const body = '{"version":1,"persona":{"name":"Ada"},"phrases":[]}';
  const file = new File([body], "Ada.json", { type: "application/json" });
  Object.defineProperty(file, "text", { value: async () => body });
  render(<PersonasPage />);
  await user.click(await screen.findByRole("button", { name: "Import persona and common phrases" }));
  await user.upload(screen.getByLabelText("Persona JSON file"), file);
  await user.type(screen.getByRole("textbox", { name: "Import as (optional)" }), "Ada from Pi");
  await user.click(screen.getByRole("button", { name: "Import" }));
  expect(await screen.findByText("Imported as Ada from Pi.")).toBeTruthy();
  expect(posted?.get("bundle")).toBe(body);
  expect(posted?.get("name")).toBe("Ada from Pi");
});
