// Preset writes and failed saves are exercised through the real accessible controls.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { Equalizer } from "../pages/Equalizer";
import { postForm, usePageData } from "../api";
import type { EqualizerView } from "../types";
vi.mock("../api", () => ({ usePageData: vi.fn(), postForm: vi.fn() }));
const profile = { Preset: "Flat", Bands: Array(10).fill(0) };
const custom = { Preset: "Custom", Bands: [4, 0, 0, 0, 0, 0, 0, 0, 0, 0] };
const view = (over: Partial<EqualizerView> = {}): EqualizerView => ({ Profile: profile, Custom: custom, Error: "", ...over });
beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(usePageData).mockReturnValue({ data: view(), error: "", loading: false, reload: vi.fn(), set: vi.fn() } as never);
});
it("shows ten bands and restores the saved Custom", async () => {
  vi.mocked(postForm).mockResolvedValue({ ok: true, status: 200, text: "", view: view({ Profile: custom }) });
  render(<Equalizer />); await userEvent.click(screen.getByRole("button", { name: /Equalizer/ }));
  await waitFor(() => expect(screen.getAllByRole("slider")).toHaveLength(10));
  await userEvent.click(screen.getByRole("button", { name: "Custom" }));
  expect(postForm).toHaveBeenCalledWith("/api/ui/equalizer", { profile: JSON.stringify(custom) }, undefined);
});
it("reports refused saves", async () => {
  vi.mocked(postForm).mockResolvedValue({ ok: false, status: 500, text: "Save failed", view: null });
  render(<Equalizer />); await userEvent.click(screen.getByRole("button", { name: /Equalizer/ }));
  await userEvent.click(await screen.findByRole("button", { name: "Bass Boost" }));
  expect(await screen.findByText("Save failed")).toBeTruthy();
});
