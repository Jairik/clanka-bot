// The Hermes page: an honest not-installed card, the surface it would get, and
// accounts whose secrets are written one field at a time.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { HermesPage } from "../pages/Hermes";
import type { HermesView, Integration } from "../types";
import { json, stubFetch } from "./harness";

const github = (over: Partial<Integration> = {}): Integration => ({
  ID: "github",
  Name: "GitHub",
  Summary: "Issues, pull requests and notifications on your own repositories.",
  Docs: "Use a fine-grained token limited to the repositories you want Hermes to have.",
  Status: "Not linked",
  StatusKey: "missing",
  Hermes: false,
  CanGrant: true,
  Linkable: true,
  Secrets: [
    { Name: "GITHUB_TOKEN", Set: false, Source: "none", Last4: "", Optional: false },
    { Name: "GITHUB_NOTIFICATIONS_TOKEN", Set: false, Source: "none", Last4: "", Optional: true },
  ],
  Tools: ["github.issue_create", "github.pr_list"],
  ...over,
});

const openrouter = (over: Partial<Integration> = {}): Integration => ({
  ID: "openrouter",
  Name: "Agent model",
  Summary: "The provider behind the Agent path.",
  Docs: "Without this key the Fast path still works.",
  Status: "From environment",
  StatusKey: "environment",
  Hermes: false,
  CanGrant: true,
  Linkable: true,
  Secrets: [{ Name: "OPENROUTER_API_KEY", Set: true, Source: "env", Last4: "", Optional: false }],
  Tools: ["agent path"],
  ...over,
});

const view = (rows: Integration[] = [github(), openrouter()]): HermesView => ({
  Owner: "owner@example.com",
  Page: "hermes",
  Flash: "",
  Error: "",
  HasRegistry: true,
  HasIntegrations: true,
  SurfaceCount: 2,
  LocalOnlyCount: 5,
  ConfirmCount: 3,
  Granted: 0,
  Surface: [
    { Name: "lights.power", Description: "Turn a light on or off.", Permission: "act" },
    { Name: "spotify.play", Description: "Start playback.", Permission: "read" },
  ],
  Rows: rows,
});

type Call = { path: string; body: URLSearchParams };

/** Answers writes with the view the test supplies and records each request. */
function stub(write: (path: string) => HermesView, get: () => HermesView = () => view()) {
  const calls: Call[] = [];
  const route = (path: string) => (init?: RequestInit) => {
    calls.push({ path, body: new URLSearchParams(String(init?.body ?? "")) });
    return json(write(path));
  };
  stubFetch({
    "/api/ui/hermes": () => json(get()),
    "/hermes/integrations/github": route("/hermes/integrations/github"),
    "/hermes/integrations/github/unlink": route("/hermes/integrations/github/unlink"),
    "/hermes/integrations/openrouter/grant": route("/hermes/integrations/openrouter/grant"),
  });
  return calls;
}

it("says Hermes is not installed and shows the surface it would get", async () => {
  stub(() => view());
  render(<HermesPage />);

  expect(await screen.findByText("Hermes is not installed")).toBeTruthy();
  expect(screen.getByText(/not installed in this build/)).toBeTruthy();
  expect(screen.getByText("2 tools")).toBeTruthy();
  expect(screen.getByText("lights.power")).toBeTruthy();
  expect(screen.getByText(/5 Tools run on the Pi/)).toBeTruthy();
  expect(screen.getByText(/3 Tools need a spoken confirmation/)).toBeTruthy();
  expect(screen.getByText("GitHub")).toBeTruthy();
  expect(screen.getByText("Agent model")).toBeTruthy();
});

it("links an account with one field per secret", async () => {
  const calls = stub((path) =>
    path === "/hermes/integrations/github"
      ? view([github({ Status: "Linked", StatusKey: "linked" }), openrouter()])
      : view(),
  );
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("GitHub");

  await user.click(screen.getByRole("button", { name: /GitHub/ }));
  await user.type(await screen.findByLabelText("GITHUB_TOKEN"), "ghp_secret");
  await user.click(screen.getByRole("button", { name: "Link account" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/hermes/integrations/github");
  expect(calls[0].body.get("GITHUB_TOKEN")).toBe("ghp_secret");
  // Optional secrets are sent empty rather than omitted.
  expect(calls[0].body.get("GITHUB_NOTIFICATIONS_TOKEN")).toBe("");
});

it("grants Hermes access from the account switch", async () => {
  const calls = stub((path) =>
    path === "/hermes/integrations/openrouter/grant"
      ? view([github(), openrouter({ Hermes: true })])
      : view(),
  );
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("Agent model");

  await user.click(screen.getByRole("button", { name: /Agent model/ }));
  await user.click(await screen.findByRole("switch", { name: "Allow Hermes to use Agent model" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/hermes/integrations/openrouter/grant");
  expect(calls[0].body.get("allowed")).toBe("on");
});

it("unlinks an account that has stored secrets", async () => {
  const linked = view([
    github({
      Status: "Linked",
      StatusKey: "linked",
      Secrets: [{ Name: "GITHUB_TOKEN", Set: true, Source: "store", Last4: "1234", Optional: false }],
    }),
    openrouter(),
  ]);
  const calls = stub(() => linked, () => linked);
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("GitHub");

  await user.click(screen.getByRole("button", { name: /GitHub/ }));
  await user.click(await screen.findByRole("button", { name: "Unlink" }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/hermes/integrations/github/unlink");
  expect(calls[0].body.toString()).toBe("");
});

it("explains an unconfigured account registry", async () => {
  stubFetch({ "/api/ui/hermes": () => json({ ...view([]), HasIntegrations: false }) });
  render(<HermesPage />);

  expect(await screen.findByText(/built without the integrations or secrets service/)).toBeTruthy();
  expect(screen.getByText("Accounts are not configured")).toBeTruthy();
});

it("shows a refused link as an error", async () => {
  stubFetch({
    "/api/ui/hermes": () => json(view()),
    "/hermes/integrations/github": () =>
      json({ ...view(), Error: "Fill every required field before saving." }, 400),
  });
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("GitHub");

  await user.click(screen.getByRole("button", { name: /GitHub/ }));
  await user.type(await screen.findByLabelText("GITHUB_TOKEN"), "x");
  await user.click(screen.getByRole("button", { name: "Link account" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("Fill every required field before saving.");
});
