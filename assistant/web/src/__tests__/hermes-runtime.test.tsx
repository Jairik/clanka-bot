// Hermes job provider: two explicit choices, a server-owned OpenCode Go model
// list, readiness cues, and a JSON save through a stubbed fetch.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { HermesRuntimeSection, initialModel, type HermesRuntimeView } from "../pages/HermesRuntime";
import { json, stubFetch } from "./harness";

const MODELS = ["deepseek-v4.1-flash", "qwen3.5-coder"];

const view = (over: Partial<HermesRuntimeView> = {}): HermesRuntimeView => ({
  provider: "local",
  open_code_go_model: "deepseek-v4.1-flash",
  open_code_go_models: MODELS,
  local_model: "Ornith",
  local_ready: true,
  open_code_go_ready: true,
  ...over,
});

type Call = { body: unknown };

function stub(get: () => Response, write: (body: { provider: string; open_code_go_model: string }) => Response = (b) => json(view(b as Partial<HermesRuntimeView>))) {
  const calls: Call[] = [];
  stubFetch({
    "/api/ui/hermes/runtime": (init) => {
      if (init?.method !== "POST") return get();
      const body = JSON.parse(String(init.body));
      calls.push({ body });
      return write(body);
    },
  });
  return calls;
}

it("shows Local and OpenCode Go as two choices with the saved one selected", async () => {
  stub(() => json(view({ local_model: "ornith-7b" })));
  render(<HermesRuntimeSection />);

  const local = await screen.findByRole("radio", { name: /Local/ });
  const cloud = screen.getByRole("radio", { name: /OpenCode Go/ });
  expect((local as HTMLInputElement).checked).toBe(true);
  expect((cloud as HTMLInputElement).checked).toBe(false);
  expect(screen.getByText("ornith-7b")).toBeTruthy();
  expect(screen.getByText("Local · ornith-7b")).toBeTruthy();
  expect(screen.getByText(/no automatic\s+fallback/)).toBeTruthy();
  expect(screen.getByText(/sends job requests to OpenCode Go/)).toBeTruthy();
  // Nothing changed yet, so there is nothing to save.
  expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled") ||
    screen.getByRole("button", { name: "Save" }).getAttribute("aria-disabled") === "true").toBe(true);
});

it("falls back to the Ornith name when the local model is unnamed", async () => {
  stub(() => json(view({ local_model: "" })));
  render(<HermesRuntimeSection />);
  expect(await screen.findByText("Local · Ornith")).toBeTruthy();
});

it("switches new jobs to OpenCode Go with the chosen model", async () => {
  const calls = stub(() => json(view()));
  const user = userEvent.setup();
  render(<HermesRuntimeSection />);

  await user.click(await screen.findByRole("radio", { name: /OpenCode Go/ }));
  await user.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].body).toEqual({ provider: "opencode-go", open_code_go_model: "deepseek-v4.1-flash" });
  expect(await screen.findByText("New jobs run on OpenCode Go.")).toBeTruthy();
  expect((screen.getByRole("radio", { name: /OpenCode Go/ }) as HTMLInputElement).checked).toBe(true);
});

it("saves a preferred cloud model while Local stays selected", async () => {
  const calls = stub(() => json(view()));
  const user = userEvent.setup();
  render(<HermesRuntimeSection />);

  await user.click(await screen.findByRole("button", { name: /OpenCode Go model/ }));
  await user.click(await screen.findByRole("option", { name: "qwen3.5-coder" }));
  await user.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].body).toEqual({ provider: "local", open_code_go_model: "qwen3.5-coder" });
  expect(await screen.findByText("New jobs run locally.")).toBeTruthy();
  expect((screen.getByRole("radio", { name: /Local/ }) as HTMLInputElement).checked).toBe(true);
});

it("offers only server-owned models, not a free-text box", async () => {
  stub(() => json(view()));
  const user = userEvent.setup();
  render(<HermesRuntimeSection />);

  await user.click(await screen.findByRole("button", { name: /OpenCode Go model/ }));
  const options = await screen.findAllByRole("option");
  expect(options.map((o) => o.textContent)).toEqual(MODELS);
  expect(screen.queryByRole("textbox")).toBeNull();
  expect(screen.getByText(/zero-day request retention and tool-call support/)).toBeTruthy();
});

it("points at Secrets when OpenCode Go has no key", async () => {
  stub(() => json(view({ open_code_go_ready: false })));
  const user = userEvent.setup();
  render(<HermesRuntimeSection />);

  const link = await screen.findByRole("link", { name: "Secrets" });
  expect(link.getAttribute("href")).toBe("/secrets");
  await user.click(screen.getByRole("radio", { name: /OpenCode Go/ }));
  expect(screen.getByText(/jobs cannot run on it until its key is set/)).toBeTruthy();
});

it("marks the section when the selected provider is not ready", async () => {
  stub(() => json(view({ provider: "opencode-go", open_code_go_ready: false })));
  render(<HermesRuntimeSection />);
  await screen.findByRole("radio", { name: /OpenCode Go/ });
  expect(screen.getAllByText("not ready").length).toBe(1);
});

it("shows the server's refusal and keeps the draft", async () => {
  const calls = stub(
    () => json(view()),
    () => json({ error: "OpenCode Go key is not set" }, 503),
  );
  const user = userEvent.setup();
  render(<HermesRuntimeSection />);

  await user.click(await screen.findByRole("radio", { name: /OpenCode Go/ }));
  await user.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect((await screen.findByRole("alert")).textContent).toContain("OpenCode Go key is not set");
  expect((screen.getByRole("radio", { name: /OpenCode Go/ }) as HTMLInputElement).checked).toBe(true);
});

it("says plainly when the assistant predates the control (404)", async () => {
  stub(() => new Response("404 page not found", { status: 404 }));
  render(<HermesRuntimeSection />);
  expect(await screen.findByText(/does not offer a job provider choice yet/)).toBeTruthy();
  expect(screen.queryByRole("radio")).toBeNull();
  expect(screen.queryByRole("button", { name: /Retry/i })).toBeNull();
});

it("preselects the saved model, then the default, then the first offered", () => {
  expect(initialModel(view({ open_code_go_model: "qwen3.5-coder" }))).toBe("qwen3.5-coder");
  expect(initialModel(view({ open_code_go_model: "retired-model" }))).toBe("deepseek-v4.1-flash");
  expect(initialModel(view({ open_code_go_model: "", open_code_go_models: ["qwen3.5-coder"] }))).toBe("qwen3.5-coder");
  expect(initialModel(view({ open_code_go_model: "", open_code_go_models: null }))).toBe("");
});

it("asks for a new choice when the saved model is no longer offered", async () => {
  stub(() => json(view({ open_code_go_model: "retired-model" })));
  render(<HermesRuntimeSection />);
  expect(await screen.findByText(/retired-model is no longer offered/)).toBeTruthy();
});
