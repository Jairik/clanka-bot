// The Monster page reads the cloud view and posts each command as JSON, then
// reloads, because the cloud is the only source of truth.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it } from "vitest";
import { MonsterPage } from "../pages/Monster";
import { json, stubFetch } from "./harness";

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

beforeEach(() => {
  posts = [];
  gets = 0;
  stubFetch({
    "/api/ui/monster": async (init) => {
      if (init?.method === "POST") {
        posts.push(JSON.parse(String(init.body)));
        return json({ ok: true });
      }
      gets++;
      return json(configured());
    },
  });
});

it("shows a light and sends power, then reloads the view", async () => {
  render(<MonsterPage />);
  await screen.findByText("Lounge bar");
  expect(screen.getByText("M-LB-1")).toBeTruthy();
  expect(gets).toBe(1);

  await userEvent.click(screen.getByRole("button", { name: "Lounge bar on" }));
  await waitFor(() => expect(posts).toContainEqual({ action: "power", device: "DSN1", value: "on" }));
  await waitFor(() => expect(gets).toBe(2));
});

it("sends the colour picked in the colour input", async () => {
  render(<MonsterPage />);
  await screen.findByText("Lounge bar");

  fireEvent.change(screen.getByLabelText("Lounge bar colour"), { target: { value: "#ff0000" } });
  await waitFor(() => expect(posts).toContainEqual({ action: "color", device: "DSN1", value: "#ff0000" }));
  await waitFor(() => expect(gets).toBe(2));
});

it("shows a configured account's cloud error instead of an empty account", async () => {
  stubFetch({
    "/api/ui/monster": () =>
      json({
        configured: true,
        status: "",
        error: "Monster rejected the email or password. Replace them on the Secrets page.",
        devices: [],
      }),
  });
  render(<MonsterPage />);

  await screen.findByText(/Monster rejected the email or password/);
  expect(screen.queryByText(/No Monster lights on this account/)).toBeNull();
});

it("points at the Secrets page when the account is not set up", async () => {
  stubFetch({
    "/api/ui/monster": () =>
      json({ configured: false, status: "Add MONSTER_EMAIL and MONSTER_PASSWORD on the Secrets page.", devices: [] }),
  });
  render(<MonsterPage />);

  const link = await screen.findByRole("link", { name: "Secrets" });
  expect(link.getAttribute("href")).toBe("/secrets");
  expect(screen.getAllByText(/MONSTER_EMAIL/).length).toBeGreaterThan(0);
  expect(screen.queryByRole("button", { name: "Lounge bar on" })).toBeNull();
  expect(screen.queryByText("Lounge bar")).toBeNull();
});
