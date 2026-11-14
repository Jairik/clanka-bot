// The Lights page loads the saved controllers and posts each command as JSON.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it } from "vitest";
import { LightsPage } from "../pages/Lights";
import { json, stubFetch } from "./harness";

const view = {
  enabled: true,
  status: "Ready to scan",
  devices: [{ address: "BE:68:F8:24:A7:2D", name: "Desk", protocol: "lotus" }],
};

let posts: unknown[];

beforeEach(() => {
  posts = [];
  stubFetch({
    "/api/ui/lights": async (init) => {
      if (init?.method === "POST") {
        posts.push(JSON.parse(String(init.body)));
        return json({ ok: true });
      }
      return json(view);
    },
  });
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
