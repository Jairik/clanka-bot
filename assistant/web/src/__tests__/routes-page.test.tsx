// The Routes page is read-only: it explains the routing order and lists every
// Tool with its path properties, all from one GET.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { RoutesPage } from "../pages/Routes";
import type { RoutesView } from "../types";
import { json, stubFetch } from "./harness";

const view = (over: Partial<RoutesView> = {}): RoutesView => ({
  Owner: "owner@example.com",
  Page: "routes",
  Flash: "",
  Error: "",
  HasRegistry: true,
  Prefixes: ["ask", "tell"],
  Filler: "One moment.",
  Unavailable: "I can't reach the model right now.",
  Totals: { Tools: 3, FastPath: 1, AgentOnly: 1, LocalOnly: 1, Confirm: 1, Hermes: 2 },
  Rows: [
    {
      Name: "lights.power",
      Description: "Turn a light on or off.",
      Permission: "act",
      FastPath: true,
      AgentOnly: false,
      LocalOnly: false,
      Confirm: false,
      Enabled: true,
      Hermes: true,
      ControlPhrases: 4,
      Examples: 2,
    },
    {
      Name: "system.status",
      Description: "Report the Pi's health.",
      Permission: "read",
      FastPath: false,
      AgentOnly: true,
      LocalOnly: true,
      Confirm: false,
      Enabled: true,
      Hermes: false,
      ControlPhrases: 0,
      Examples: 1,
    },
    {
      Name: "spotify.play",
      Description: "Start playback.",
      Permission: "confirm",
      FastPath: false,
      AgentOnly: false,
      LocalOnly: false,
      Confirm: true,
      Enabled: false,
      Hermes: false,
      ControlPhrases: 2,
      Examples: 3,
    },
  ],
  ...over,
});

it("explains the paths, the order and the live wording", async () => {
  stubFetch({ "/api/ui/routes": () => json(view()) });
  render(<RoutesPage />);

  // The legend says what each badge means.
  expect(await screen.findByText(/Deterministic: a matching control phrase/)).toBeTruthy();
  expect(screen.getByText(/only the Agent calls it/)).toBeTruthy();
  expect(screen.getByText(/never leave the house/)).toBeTruthy();
  expect(screen.getByText(/part of the surface Hermes would get/)).toBeTruthy();

  expect(screen.getByText("Routing order")).toBeTruthy();
  expect(screen.getByText("Pending Confirmation")).toBeTruthy();
  expect(screen.getByText("Control phrase / Focus intent")).toBeTruthy();
  expect(screen.getByText("One moment.")).toBeTruthy();
  expect(screen.getByText("I can't reach the model right now.")).toBeTruthy();
  expect(screen.getByText("ask")).toBeTruthy();
  // Hermes is counted, so the page says no adapter is installed.
  expect(screen.getByText(/Hermes is not installed/)).toBeTruthy();
});

it("lists each Tool with its permission, badges and live counts", async () => {
  stubFetch({ "/api/ui/routes": () => json(view()) });
  render(<RoutesPage />);

  expect(await screen.findByText("lights.power")).toBeTruthy();
  expect(screen.getByText("system.status")).toBeTruthy();
  expect(screen.getByText("4 phrases · 2 examples")).toBeTruthy();
  expect(screen.getByText("Disabled")).toBeTruthy();
  // Permissions group the list.
  expect(screen.getByRole("heading", { name: /Read/ })).toBeTruthy();
  expect(screen.getByRole("heading", { name: /Confirm/ })).toBeTruthy();
});

it("filters the list by name", async () => {
  stubFetch({ "/api/ui/routes": () => json(view()) });
  const user = userEvent.setup();
  render(<RoutesPage />);
  await screen.findByText("lights.power");

  await user.type(screen.getByLabelText("Filter tools"), "spotify");

  expect(screen.getByText("spotify.play")).toBeTruthy();
  expect(screen.queryByText("lights.power")).toBeNull();
  expect(screen.queryByText("system.status")).toBeNull();
  expect(screen.getByText("1 of 3")).toBeTruthy();
});

it("explains an unconfigured registry", async () => {
  stubFetch({ "/api/ui/routes": () => json(view({ HasRegistry: false, Rows: [], Totals: { Tools: 0, FastPath: 0, AgentOnly: 0, LocalOnly: 0, Confirm: 0, Hermes: 0 } })) });
  render(<RoutesPage />);

  expect(await screen.findByText(/built without the Tool registry/)).toBeTruthy();
  expect(screen.queryByText("Routing order")).toBeNull();
});

it("shows a failed read", async () => {
  stubFetch({ "/api/ui/routes": () => new Response("The registry did not answer.", { status: 502 }) });
  render(<RoutesPage />);

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("The registry did not answer.");
});
