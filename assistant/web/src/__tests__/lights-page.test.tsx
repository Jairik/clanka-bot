// The Lights page carries both lighting transports: the Bluetooth room
// controllers ("lights") and the Monster cloud lights ("monster"). Each half
// reads its own view, so this suite stubs both endpoints on every render and
// checks that neither half can hide the other.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it } from "vitest";
import { LightsPage } from "../pages/Lights";
import { json, stubFetch, type Route } from "./harness";

const lights = {
  enabled: true,
  status: "Ready to scan",
  devices: [{ address: "BE:68:F8:24:A7:2D", name: "Desk", protocol: "lotus" }],
};

const bar = {
  dsn: "DSN1",
  name: "Lounge bar",
  model: "M-LB-1",
  on: true,
  mode: "Colour",
  brightness: 80,
  color: "#ffffff",
  kelvin: 4000,
  effects: [{ id: "e1", name: "Sunrise" }],
};

const monster = { configured: true, status: "1 light", devices: [bar] };

let posts: unknown[];

/** Both transports, with either half overridable for the failure cases. */
const stub = (over: Record<string, Route> = {}) =>
  stubFetch({
    "/api/ui/lights": async (init) => {
      if (init?.method === "POST") {
        posts.push(JSON.parse(String(init.body)));
        return json({ ok: true });
      }
      return json(lights);
    },
    "/api/ui/monster": () => json(monster),
    ...over,
  });

beforeEach(() => {
  posts = [];
  stub();
});

it("shows the empty list only after a successful read", async () => {
  // A failed initial read must not imply that the user has no saved lights.
  let attempts = 0;
  stub({ "/api/ui/lights": () => (++attempts === 1 ? json({}, 503) : json({ ...lights, devices: [] })) });
  render(<LightsPage />);

  const room = within(screen.getByRole("region", { name: "Room lights" }));
  expect(room.getByRole("status").textContent).toMatch(/Loading/);
  expect(room.queryByText(/No lights saved/)).toBeNull();
  expect(await room.findByRole("alert")).toBeTruthy();
  expect(room.queryByText(/No lights saved/)).toBeNull();

  await userEvent.click(room.getByRole("button", { name: "Retry" }));
  expect(await room.findByText(/No lights saved/)).toBeTruthy();
});

it("shows a saved light and sends power", async () => {
  render(<LightsPage />);
  await screen.findByText("Desk");
  await userEvent.click(screen.getByRole("button", { name: "Desk on" }));
  await waitFor(() => expect(posts).toContainEqual({ action: "power", device: "BE:68:F8:24:A7:2D", value: "on" }));
});

it("turns the feature off from the page switch", async () => {
  render(<LightsPage />);
  await screen.findByText("Desk");
  await userEvent.click(screen.getByRole("switch", { name: "Enable lights" }));
  await waitFor(() => expect(posts).toContainEqual({ action: "enable", value: "false" }));
});

it("renders the room and desk transports from one render", async () => {
  render(<LightsPage />);

  expect(await screen.findByRole("heading", { name: "Room lights" })).toBeTruthy();
  expect(await screen.findByRole("heading", { name: "PC / desk lights" })).toBeTruthy();
  // One render, both views: the Bluetooth light and the cloud light together.
  expect(await screen.findByRole("button", { name: "Desk on" })).toBeTruthy();
  expect(await screen.findByRole("button", { name: "Lounge bar on" })).toBeTruthy();
  expect(screen.getByText("State comes from the Monster cloud.")).toBeTruthy();
});

it("shows a failed Monster read without hiding the Bluetooth controls", async () => {
  stub({ "/api/ui/monster": () => new Response("The Monster cloud did not answer.", { status: 502 }) });
  render(<LightsPage />);

  await screen.findByRole("button", { name: "Desk on" });
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("The Monster cloud did not answer.");
  // One failed endpoint, one alert; the room half has its own retry.
  expect(screen.getAllByRole("alert")).toHaveLength(1);

  await userEvent.click(screen.getByRole("button", { name: "Desk on" }));
  await waitFor(() => expect(posts).toContainEqual({ action: "power", device: "BE:68:F8:24:A7:2D", value: "on" }));
});

it("shows a failed Bluetooth read without hiding the Monster lights", async () => {
  stub({ "/api/ui/lights": () => new Response("Bluetooth is unavailable.", { status: 500 }) });
  render(<LightsPage />);

  await screen.findByRole("button", { name: "Lounge bar on" });
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("Bluetooth is unavailable.");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
});
