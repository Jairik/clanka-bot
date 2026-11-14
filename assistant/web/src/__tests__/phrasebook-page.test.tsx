// The Phrase book page: one row per Tool, a save that posts the Tool and the
// phrase, and a generation that answers with the refreshed view.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it } from "vitest";
import { PhrasebookPage } from "../pages/Phrasebook";
import type { PhrasebookView } from "../types";
import { json, stubFetch } from "./harness";

const view = (over: Partial<PhrasebookView> = {}): PhrasebookView => ({
  Owner: "owner@example.com",
  Page: "phrasebook",
  Flash: "",
  Error: "",
  HasService: true,
  HasGenerator: true,
  ActivePersona: "Ada",
  Selected: "Ada",
  Personas: [
    { Name: "Ada", Active: true, Count: 1 },
    { Name: "Bo", Active: false, Count: 0 },
  ],
  Slots: [
    { Tool: "lights.power", Group: "lights", Description: "Turn a light on or off.", Default: "Done.", Phrase: "Done.", Source: "default", Overridden: false },
    { Tool: "timer.set", Group: "timer", Description: "Set a timer.", Default: "Set.", Phrase: "Timer's ticking.", Source: "manual", Overridden: true },
  ],
  Total: 2,
  Overridden: 1,
  Generated: 0,
  Manual: 1,
  ...over,
});

type Call = { path: string; method: string; body: URLSearchParams };

let calls: Call[];

/** Records writes and answers each with the view the test supplies. */
function stub(write: (path: string) => PhrasebookView, get: () => PhrasebookView = () => view()) {
  calls = [];
  const writeRoute = (path: string) => (init?: RequestInit) => {
    calls.push({ path, method: init?.method ?? "GET", body: new URLSearchParams(String(init?.body ?? "")) });
    return json(write(path));
  };
  stubFetch({
    "/api/ui/phrasebook": () => json(get()),
    "/phrasebook/Ada": writeRoute("/phrasebook/Ada"),
    "/phrasebook/Ada/generate": writeRoute("/phrasebook/Ada/generate"),
    "/phrasebook/Bo": writeRoute("/phrasebook/Bo"),
  });
}

beforeEach(() => stub(() => view()));

it("groups the phrases and shows the counts and the active persona", async () => {
  render(<PhrasebookPage />);

  expect(await screen.findByText("lights.power")).toBeTruthy();
  expect(screen.getByText("timer.set")).toBeTruthy();
  // The group headings carry the Tool prefix.
  expect(screen.getByText("Lights")).toBeTruthy();
  expect(screen.getByText("Timer")).toBeTruthy();
  // The active persona is marked.
  const ada = screen.getByRole("button", { name: /Show Ada \(active persona\), 1 override/ });
  expect(ada.getAttribute("aria-pressed")).toBe("true");
  expect(screen.getAllByText("1 phrase")).toHaveLength(2);
});

it("saves an edited phrase with the Tool name and the new text", async () => {
  stub(() =>
    view({
      Slots: [
        { Tool: "lights.power", Group: "lights", Description: "Turn a light on or off.", Default: "Done.", Phrase: "Lights on.", Source: "manual", Overridden: true },
        { Tool: "timer.set", Group: "timer", Description: "Set a timer.", Default: "Set.", Phrase: "Timer's ticking.", Source: "manual", Overridden: true },
      ],
      Overridden: 2,
    }),
  );  const user = userEvent.setup();
  render(<PhrasebookPage />);
  await screen.findByText("lights.power");

  const field = screen.getByLabelText("lights.power phrase") as HTMLInputElement;
  await user.clear(field);
  await user.type(field, "Lights on.");

  await user.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/phrasebook/Ada");
  expect(calls[0].method).toBe("POST");
  expect(calls[0].body.get("tool")).toBe("lights.power");
  expect(calls[0].body.get("phrase")).toBe("Lights on.");
});

it("asks for a whole generated set with no body", async () => {
  stub(() => view({ Flash: "Phrase book generated. Review the rows before the next request." }));
  const user = userEvent.setup();
  render(<PhrasebookPage />);
  await screen.findByText("lights.power");

  await user.click(screen.getByRole("button", { name: "Generate from persona" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/phrasebook/Ada/generate");
  expect(calls[0].method).toBe("POST");
  expect(String(calls[0].body)).toBe("");
  expect(await screen.findByText(/Phrase book generated/)).toBeTruthy();});

it("switches persona through a read of the same view", async () => {
  const seen: string[] = [];
  stubFetch({
    "/api/ui/phrasebook": () => json(view()),
    "/api/ui/phrasebook?persona=Bo": () => {
      seen.push("Bo");
      return json(view({ Selected: "Bo", ActivePersona: "Ada", Overridden: 0, Manual: 0 }));
    },
  });
  const user = userEvent.setup();
  render(<PhrasebookPage />);
  await screen.findByText("lights.power");

  await user.click(screen.getByRole("button", { name: /^Show Bo,/ }));

  await waitFor(() => expect(seen).toEqual(["Bo"]));
  await waitFor(() => expect(screen.getByRole("button", { name: /^Show Bo,/ }).getAttribute("aria-pressed")).toBe("true"));
});

it("shows the shipped defaults read-only when no persona is active", async () => {
  const defaults = view({
    ActivePersona: "",
    Selected: "",
    Personas: [{ Name: "Ada", Active: false, Count: 1 }],
    Slots: [
      { Tool: "lights.power", Group: "lights", Description: "Turn a light on or off.", Default: "Done.", Phrase: "Done.", Source: "default", Overridden: false },
    ],
    Total: 1,
    Overridden: 0,
    Generated: 0,
    Manual: 0,
  });
  stub(() => defaults, () => defaults);
  render(<PhrasebookPage />);

  expect(await screen.findByText(/No Persona is active/)).toBeTruthy();
  expect(screen.getByText("Done.")).toBeTruthy();
  // Nothing on the page can write without a persona to write to.
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Generate from persona" })).toBeNull();
});

it("explains an unconfigured phrase book", async () => {
  const none = view({ HasService: false, Personas: [], Slots: [], Total: 0, Overridden: 0 });
  stub(() => none, () => none);
  render(<PhrasebookPage />);

  expect(await screen.findByText(/built without the phrase book/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Generate from persona" })).toBeNull();
});

it("shows a refused save as an error and keeps the page", async () => {
  calls = [];
  stubFetch({
    "/api/ui/phrasebook": () => json(view()),
    "/phrasebook/Ada": (init) => {
      calls.push({ path: "/phrasebook/Ada", method: init?.method ?? "GET", body: new URLSearchParams(String(init?.body ?? "")) });
      return json(view({ Error: "A phrase must be one non-empty line up to 80 characters." }), 400);
    },
  });
  const user = userEvent.setup();
  render(<PhrasebookPage />);
  await screen.findByText("lights.power");

  const field = screen.getByLabelText("lights.power phrase") as HTMLInputElement;
  await user.clear(field);
  await user.type(field, "x");
  await user.click(screen.getByRole("button", { name: "Save" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("one non-empty line up to 80 characters");
});
