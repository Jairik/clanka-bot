// The Monster half of the unified Lights page: it reads the cloud view and
// posts each command as JSON, then reloads, because the cloud is the only
// source of truth. The Bluetooth half is stubbed quiet on every render, since
// the one page reads both endpoints.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it } from "vitest";
import { LightsPage } from "../pages/Lights";
import { json, stubFetch, type Route } from "./harness";

const device = {
  dsn: "DSN1",
  name: "Lounge bar",
  model: "M-LB-1",
  on: true,
  mode: "Colour",
  brightness: 80,
  color: "#ffffff",
  kelvin: 4000,
  effects: [
    { id: "e1", name: "Sunrise" },
    { id: "e2", name: "Aurora" },
  ],
};

let posts: unknown[];
let gets: number;

const configured = (devices = [device]) => ({ configured: true, status: `${devices.length} light`, devices });

/** The Bluetooth view is present but quiet, so only the cloud half is under test. */
const stub = (monster: Route) =>
  stubFetch({
    "/api/ui/lights": () => json({ enabled: false, status: "", devices: [] }),
    "/api/ui/monster": monster,
  });

beforeEach(() => {
  posts = [];
  gets = 0;
  stub(async (init) => {
    if (init?.method === "POST") {
      posts.push(JSON.parse(String(init.body)));
      return json({ ok: true });
    }
    gets++;
    return json(configured());
  });
});

it("shows a light and sends power, then reloads the view", async () => {
  render(<LightsPage />);
  await screen.findByText("Lounge bar");
  expect(screen.getByText("M-LB-1")).toBeTruthy();
  expect(gets).toBe(1);

  await userEvent.click(screen.getByRole("button", { name: "Lounge bar on" }));
  await waitFor(() => expect(posts).toContainEqual({ action: "power", device: "DSN1", value: "on" }));
  await waitFor(() => expect(gets).toBe(2));
});

it("sends the colour picked in the colour input", async () => {
  render(<LightsPage />);
  await screen.findByText("Lounge bar");

  fireEvent.change(screen.getByLabelText("Lounge bar colour"), { target: { value: "#ff0000" } });
  await waitFor(() => expect(posts).toContainEqual({ action: "color", device: "DSN1", value: "#ff0000" }));
  await waitFor(() => expect(gets).toBe(2));
});

it("shows a refused write and still reloads the view", async () => {
  stub(async (init) => {
    if (init?.method === "POST") return new Response("Monster rejected the email or password.", { status: 502 });
    gets++;
    return json(configured());
  });
  render(<LightsPage />);
  await screen.findByText("Lounge bar");

  await userEvent.click(screen.getByRole("button", { name: "Lounge bar on" }));
  await screen.findByText("Monster rejected the email or password.");
  await waitFor(() => expect(gets).toBe(2));
});

it("shows a configured account's cloud error instead of an empty account", async () => {
  stub(() =>
    json({
      configured: true,
      status: "",
      error: "Monster rejected the email or password. Replace them on the Secrets page.",
      devices: [],
    }),
  );
  render(<LightsPage />);

  await screen.findByText(/Monster rejected the email or password/);
  expect(screen.queryByText(/No Monster lights on this account/)).toBeNull();
});

it("points at the Secrets page when the account is not set up", async () => {
  stub(() => json({ configured: false, status: "Add MONSTER_EMAIL and MONSTER_PASSWORD on the Secrets page.", devices: [] }));
  render(<LightsPage />);

  const link = await screen.findByRole("link", { name: "Secrets" });
  expect(link.getAttribute("href")).toBe("/secrets");
  expect(screen.getAllByText(/MONSTER_EMAIL/).length).toBeGreaterThan(0);
  expect(screen.queryByRole("button", { name: "Lounge bar on" })).toBeNull();
  expect(screen.queryByText("Lounge bar")).toBeNull();
});
