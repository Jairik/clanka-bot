// The Hermes page: runtime setup, the surface it would get, and
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
}) as Integration;

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
}) as Integration;

const view = (rows: Integration[] = [github(), openrouter()], over: Partial<HermesView> = {}): HermesView => ({
  Owner: "owner@example.com",
  Page: "hermes",
  Flash: "",
  Error: "",
  HasRegistry: true,
  HasIntegrations: true,
  RuntimeReady: false,
  AdapterReady: true,
  SchemasDeferred: true,
  SurfaceCount: 2,
  WithheldCount: 0,
  LocalOnlyCount: 5,
  ConfirmCount: 3,
  Granted: 0,
  Surface: [
    { Name: "weather.current", Description: "Current weather.", Permission: "read" },
    { Name: "weather.forecast", Description: "Daily forecast.", Permission: "read" },
  ],
  Withheld: [],
  Rows: rows,
  ...over,
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
    "/hermes/integrations/github/verify": route("/hermes/integrations/github/verify"),
    "/hermes/integrations/openrouter/grant": route("/hermes/integrations/openrouter/grant"),
  });
  return calls;
}

it("shows the setup required and the enforced allowlist", async () => {
  stub(() => view());
  render(<HermesPage />);

  expect(await screen.findByText("Hermes needs setup")).toBeTruthy();
  expect(screen.getByText(/Enable the private sidecar/)).toBeTruthy();
  expect(screen.getByText(/enforces the allowlist and live account grants/)).toBeTruthy();
  expect(screen.getByText("2 tools")).toBeTruthy();
  expect(screen.getByText("weather.current")).toBeTruthy();
  expect(screen.getByText(/5 Tools run on the Pi/)).toBeTruthy();
  expect(screen.getByText(/3 Tools need a spoken confirmation/)).toBeTruthy();
  expect(screen.getByText("GitHub")).toBeTruthy();
  expect(screen.getByText("Agent model")).toBeTruthy();
});

it("names withheld allowlisted tools until the grant is on", async () => {
  stubFetch({
    "/api/ui/hermes": () =>
      json(
        view([], {
          Surface: [],
          SurfaceCount: 0,
          WithheldCount: 2,
          Withheld: [
            { Name: "weather.current", Description: "Current weather.", Permission: "read" },
            { Name: "weather.forecast", Description: "Daily forecast.", Permission: "read" },
          ],
        }),
      ),
  });
  render(<HermesPage />);
  expect(await screen.findByText(/2 allowlisted Tools are withheld/)).toBeTruthy();
  expect(screen.getByText(/weather.current, weather.forecast/)).toBeTruthy();
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

it("keeps the link button enabled when a required secret is already stored but left blank", async () => {
  const calls: Call[] = [];
  const route = (path: string) => (init?: RequestInit) => {
    calls.push({ path, body: new URLSearchParams(String(init?.body ?? "")) });
    return json(view());
  };
  stubFetch({
    "/api/ui/hermes": () => json(view()),
    "/hermes/integrations/openrouter": route("/hermes/integrations/openrouter"),
  });
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("Agent model");

  // The account already stores a required secret (Set: true). Leaving the field
  // blank must not disable the button, and the POST must still carry the blank.
  await user.click(screen.getByRole("button", { name: /Agent model/ }));
  const replace = await screen.findByRole("button", { name: "Replace values" });
  expect((replace as HTMLButtonElement).disabled).toBe(false);

  await user.click(replace);
  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/hermes/integrations/openrouter");
  expect(calls[0].body.get("OPENROUTER_API_KEY")).toBe("");
});

it("disables the link button when a required secret is not set and left blank", async () => {
  stub(() => view());
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("GitHub");

  // The required secret is not stored (Set: false) and is left blank, so the
  // account cannot be linked and the save hint says so.
  await user.click(screen.getByRole("button", { name: /GitHub/ }));
  const link = await screen.findByRole("button", { name: "Link account" });
  expect((link as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText("Fill every required field to save.")).toBeTruthy();
});

it("verifies an account and renders the result inline", async () => {
  const verified = view([
    github({ verify: { class: "ok", reason: "The stored key works." } }),
    openrouter(),
  ]);
  const calls = stub((path) => (path === "/hermes/integrations/github/verify" ? verified : view()));
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("GitHub");

  await user.click(screen.getByRole("button", { name: /GitHub/ }));
  await user.click(await screen.findByRole("button", { name: /Verify/ }));

  await waitFor(() => expect(calls).toHaveLength(1));
  expect(calls[0].path).toBe("/hermes/integrations/github/verify");
  expect(await screen.findByText("Works")).toBeTruthy();
  expect(screen.getByText("The stored key works.")).toBeTruthy();
});

it("renders a rejected verify result with its label", async () => {
  const rejected = view([
    github({ verify: { class: "rejected", reason: "The stored credentials were rejected." } }),
    openrouter(),
  ]);
  stub((path) => (path === "/hermes/integrations/github/verify" ? rejected : view()));
  const user = userEvent.setup();
  render(<HermesPage />);
  await screen.findByText("GitHub");

  await user.click(screen.getByRole("button", { name: /GitHub/ }));
  await user.click(await screen.findByRole("button", { name: /Verify/ }));

  expect(await screen.findByText("Rejected")).toBeTruthy();
  expect(screen.getByText("The stored credentials were rejected.")).toBeTruthy();
});

it("does not verify on load or on a timer", async () => {
  const calls = stub(() => view());
  render(<HermesPage />);
  await screen.findByText("GitHub");
  // Give any stray effect a chance to fire; it must not.
  await new Promise((r) => setTimeout(r, 50));
  expect(calls.filter((c) => c.path.endsWith("/verify"))).toHaveLength(0);
});
