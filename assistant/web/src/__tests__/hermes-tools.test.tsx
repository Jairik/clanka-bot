// Hermes Tool recipes: list vetted Tools and saved recipes, and create, edit,
// toggle and delete a recipe through the JSON API with a stubbed fetch.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import {
  HermesToolsSection,
  apiError,
  argumentsBytes,
  parseArguments,
  validateDraft,
  type HermesRecipe,
  type HermesToolsView,
} from "../pages/HermesTools";
import { json, stubFetch } from "./harness";

const AVAILABLE = [
  { name: "weather.current", description: "Current weather." },
  { name: "calendar.today", description: "Today's events." },
];

const recipe = (over: Partial<HermesRecipe> = {}): HermesRecipe => ({
  name: "weather_home",
  description: "Weather at home",
  base_tool: "weather.current",
  arguments: { city: "Austin" },
  enabled: true,
  ...over,
});

const view = (recipes: HermesRecipe[] = [recipe()]): HermesToolsView => ({ available: AVAILABLE, recipes });

type Call = { path: string; init?: RequestInit; body: unknown };

function stub(write: (path: string, body: unknown) => Response, get: () => Response = () => json(view())) {
  const calls: Call[] = [];
  const route = (path: string) => (init?: RequestInit) => {
    const raw = init?.body;
    const body = typeof raw === "string" ? JSON.parse(raw) : null;
    calls.push({ path, init, body });
    return write(path, body);
  };
  stubFetch({
    "/api/ui/hermes/tools": (init) => (init?.method === "POST" ? route("/api/ui/hermes/tools")(init) : get()),
    "/api/ui/hermes/tools/weather_home/delete": route("/api/ui/hermes/tools/weather_home/delete"),
  });
  return calls;
}

it("lists vetted Tools and saved recipes", async () => {
  stub(() => json(view()));
  render(<HermesToolsSection />);

  const tools = await screen.findByRole("list", { name: "Vetted read-only Tools" });
  expect(within(tools).getByText("weather.current")).toBeTruthy();
  expect(within(tools).getByText("calendar.today")).toBeTruthy();
  const saved = screen.getByRole("list", { name: "Saved recipes" });
  expect(within(saved).getAllByText("weather_home").length).toBeGreaterThan(0);
  expect(within(saved).getByText("Weather at home")).toBeTruthy();
  expect(screen.getByText("1 of 1 on · 2 vetted")).toBeTruthy();
});

it("creates a recipe with fixed JSON arguments as a JSON POST", async () => {
  const created = recipe({ name: "weather_office", description: "Office weather", arguments: { city: "Dallas" } });
  const calls = stub(() => json(view([recipe(), created])));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });

  await user.click(screen.getByRole("button", { name: /Add a recipe/ }));
  const form = screen.getByRole("form", { name: "New recipe" });
  await user.type(within(form).getByLabelText("Name"), "weather_office");
  await user.type(within(form).getByLabelText("Description"), "Office weather");
  await user.click(within(form).getByLabelText("Fixed arguments (optional JSON)"));
  await user.paste('{"city": "Dallas"}');
  await user.click(within(form).getByRole("button", { name: "Add recipe" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/api/ui/hermes/tools");
  expect(new Headers(calls[0].init?.headers).get("Content-Type")).toBe("application/json");
  expect(calls[0].body).toEqual({
    name: "weather_office",
    description: "Office weather",
    base_tool: "weather.current",
    arguments: { city: "Dallas" },
    enabled: true,
  });
  expect(await screen.findByText("weather_office added.")).toBeTruthy();
  expect(screen.getByText("Office weather")).toBeTruthy();
});

it("blocks a save with invalid fields and never posts", async () => {
  const calls = stub(() => json(view()));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });

  await user.click(screen.getByRole("button", { name: /Add a recipe/ }));
  const form = screen.getByRole("form", { name: "New recipe" });
  await user.type(within(form).getByLabelText("Name"), "weather_home");
  await user.click(within(form).getByLabelText("Fixed arguments (optional JSON)"));
  await user.paste("[1, 2]");
  await user.click(within(form).getByRole("button", { name: "Add recipe" }));

  expect(within(form).getByText("A recipe with this name already exists.")).toBeTruthy();
  expect(within(form).getByText("Description is required.")).toBeTruthy();
  expect(within(form).getByText(/Arguments must be a JSON object/)).toBeTruthy();
  expect(within(form).getByRole("alert").textContent).toMatch(/Fix the highlighted fields/);
  expect(calls).toHaveLength(0);
});

it("edits a recipe without letting the name change", async () => {
  const calls = stub(() => json(view([recipe({ description: "Home weather now" })])));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });

  await user.click(screen.getByRole("button", { name: "Edit recipe weather_home" }));
  const form = screen.getByRole("form", { name: "Edit recipe weather_home" });
  expect(within(form).queryByLabelText("Name")).toBeNull();
  const desc = within(form).getByLabelText("Description");
  await user.clear(desc);
  await user.type(desc, "Home weather now");
  await user.click(within(form).getByRole("button", { name: "Save" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].body).toMatchObject({ name: "weather_home", description: "Home weather now", arguments: { city: "Austin" } });
  expect(await screen.findByText("weather_home saved.")).toBeTruthy();
});

it("disables a recipe from its switch", async () => {
  const calls = stub(() => json(view([recipe({ enabled: false })])));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });

  await user.click(screen.getByRole("switch", { name: "Enable recipe weather_home" }));
  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].body).toMatchObject({ name: "weather_home", enabled: false });
  expect(await screen.findByText("weather_home disabled.")).toBeTruthy();
  expect(screen.getByText("off")).toBeTruthy();
});

it("deletes a recipe only after confirming", async () => {
  const calls = stub(() => json(view([])));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });

  await user.click(screen.getByRole("button", { name: "Edit recipe weather_home" }));
  await user.click(screen.getByRole("button", { name: "Delete recipe weather_home" }));
  expect(calls).toHaveLength(0);
  await user.click(screen.getByRole("button", { name: "Confirm delete recipe weather_home" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/api/ui/hermes/tools/weather_home/delete");
  expect(await screen.findByText("No recipe saved yet.")).toBeTruthy();
});

it("shows the server's refusal and keeps the draft", async () => {
  stub(() => new Response("base_tool is not vetted", { status: 400, headers: { "Content-Type": "text/plain" } }));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });

  await user.click(screen.getByRole("button", { name: /Add a recipe/ }));
  const form = screen.getByRole("form", { name: "New recipe" });
  await user.type(within(form).getByLabelText("Name"), "weather_office");
  await user.type(within(form).getByLabelText("Description"), "Office weather");
  await user.click(within(form).getByRole("button", { name: "Add recipe" }));

  expect((await screen.findByRole("alert")).textContent).toMatch("base_tool is not vetted");
  expect((within(form).getByLabelText("Name") as HTMLInputElement).value).toBe("weather_office");
});

it("reads the assistant's JSON error from a refused write", async () => {
  // The assistant answers a refusal as {"error": ...} with no view.
  stub(() => json({ error: "invalid recipe: at most 64 recipes can be saved" }, 400));
  const user = userEvent.setup();
  render(<HermesToolsSection />);
  await screen.findByRole("switch", { name: "Enable recipe weather_home" });
  await user.click(screen.getByRole("switch", { name: "Enable recipe weather_home" }));
  expect((await screen.findByRole("alert")).textContent).toMatch("invalid recipe: at most 64 recipes can be saved");
  // The list stays as it was; the refusal is not mistaken for a view.
  expect(screen.getByRole("switch", { name: "Enable recipe weather_home" })).toBeTruthy();
});

it("shows the message, not the JSON, when the first read is refused", async () => {
  stub(
    () => json(view()),
    () => json({ error: "The Hermes tool manager is unavailable." }, 503),
  );
  render(<HermesToolsSection />);
  expect((await screen.findByRole("alert")).textContent).toBe("The Hermes tool manager is unavailable.");
  expect(screen.queryByText(/"error"/)).toBeNull();
});

it("offers a retry when the first read fails, and explains a missing route", async () => {
  let fail = true;
  stub(
    () => json(view()),
    () => (fail ? new Response("boom", { status: 500 }) : json(view())),
  );
  const user = userEvent.setup();
  const { unmount } = render(<HermesToolsSection />);
  expect((await screen.findByRole("alert")).textContent).toMatch("boom");
  fail = false;
  await user.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("switch", { name: "Enable recipe weather_home" })).toBeTruthy();
  unmount();

  stub(() => json(view()), () => new Response("not found", { status: 404 }));
  render(<HermesToolsSection />);
  expect(await screen.findByText(/does not serve Tool recipes yet/)).toBeTruthy();
});

it("parses fixed arguments strictly", () => {
  expect(parseArguments("")).toEqual({ value: null, error: "" });
  expect(parseArguments('{"a": 1}').value).toEqual({ a: 1 });
  expect(parseArguments("{").error).toBe("Arguments must be valid JSON.");
  expect(parseArguments("null").error).toMatch(/JSON object/);
});

const ctx = { creating: true, recipes: [recipe()], available: AVAILABLE };
const draft = (over: Partial<Parameters<typeof validateDraft>[0]> = {}) => ({
  name: "office_weather",
  description: "Office weather",
  base_tool: "weather.current",
  arguments: "",
  enabled: true,
  ...over,
});

it("validates names with the assistant's recipe rule", () => {
  for (const ok of ["a", "weather_2", "x".repeat(56), "trailing_"]) {
    expect(validateDraft(draft({ name: ok }), ctx).name, ok).toBeUndefined();
  }
  for (const bad of ["weather.home", "Weather", "2fast", "_lead", "a-b", "x".repeat(57)]) {
    expect(validateDraft(draft({ name: bad }), ctx).name, bad).toBeTruthy();
  }
  expect(validateDraft(draft({ name: "double__under" }), ctx).name).toBe("Use single underscores only.");
  expect(validateDraft(draft({ name: "weather_home" }), ctx).name).toMatch(/already exists/);
  expect(validateDraft(draft({ name: "weather_home" }), { ...ctx, creating: false }).name).toBeUndefined();
});

it("validates descriptions as one trimmed line of at most 500 characters", () => {
  expect(validateDraft(draft({ description: "  padded  " }), ctx).description).toBeUndefined();
  expect(validateDraft(draft({ description: "   " }), ctx).description).toBe("Description is required.");
  expect(validateDraft(draft({ description: "two\nlines" }), ctx).description).toMatch(/one line/);
  expect(validateDraft(draft({ description: "tab\there" }), ctx).description).toMatch(/one line/);
  // Counted in characters (runes), not UTF-16 units, and after trimming.
  expect(validateDraft(draft({ description: "😀".repeat(500) }), ctx).description).toBeUndefined();
  expect(validateDraft(draft({ description: ` ${"a".repeat(500)} ` }), ctx).description).toBeUndefined();
  expect(validateDraft(draft({ description: "a".repeat(501) }), ctx).description).toMatch(/500 characters/);
});

it("caps fixed arguments at the assistant's 8 KiB of compact JSON", () => {
  expect(argumentsBytes({ city: "Oslo" })).toBe(15);
  expect(argumentsBytes({ q: "<&>" })).toBe(`{"q":"\\u003c\\u0026\\u003e"}`.length);
  expect(argumentsBytes({ q: "é" })).toBe(10);
  expect(argumentsBytes({ q: "\u2028" })).toBe(14); // Go writes {"q":"\u2028"}
  const fits = JSON.stringify({ q: "a".repeat(8192 - 8) });
  expect(validateDraft(draft({ arguments: fits }), ctx).arguments).toBeUndefined();
  const over = JSON.stringify({ q: "a".repeat(8192 - 7) });
  expect(validateDraft(draft({ arguments: over }), ctx).arguments).toMatch(/8192 bytes/);
  // Pretty-printed input is measured as the compact JSON that is sent.
  expect(validateDraft(draft({ arguments: JSON.stringify(JSON.parse(fits), null, 8) }), ctx).arguments).toBeUndefined();
});

it("reads only a string error field from an assistant refusal", () => {
  expect(apiError({ error: "invalid recipe: nope" })).toBe("invalid recipe: nope");
  expect(apiError('{"error":"Send a single valid JSON recipe."}\n')).toBe("Send a single valid JSON recipe.");
  expect(apiError("plain text")).toBe("");
  expect(apiError({ error: 4 })).toBe("");
  expect(apiError(["error"])).toBe("");
});
