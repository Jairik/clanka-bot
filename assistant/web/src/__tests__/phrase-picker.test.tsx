// The phrase picker: the `!` mark and its label, Add phrase, and removal.
import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { PhrasePicker } from "../pages/VoiceSamples";
import type { VoiceSamplePhrase, VoiceSamplesView } from "../types";

const phrase = (over: Partial<VoiceSamplePhrase> & { Slug: string }): VoiceSamplePhrase => ({
  Text: over.Slug.replace(/-/g, " "),
  Selected: false,
  Builtin: false,
  Stored: true,
  Positives: 0,
  Negatives: 0,
  Model: "missing",
  Missing: null,
  NeedsAttention: false,
  StatusLabel: "Samples complete and a trained model is served",
  CanRemove: false,
  RemoveAction: "",
  ...over,
});

const view = (phrases: VoiceSamplePhrase[]) =>
  ({
    Slug: phrases.find((p) => p.Selected)?.Slug ?? "clanker",
    Phrase: "clanker",
    Phrases: phrases,
    AddPhraseAction: "/voice-samples/phrases",
    ModelNote: "A phrase has a trained model when the wakeword service advertises it.",
  }) as VoiceSamplesView;

it("marks a phrase that needs attention with a labelled !", () => {
  render(
    <PhrasePicker
      data={view([
        phrase({ Slug: "clanker", Builtin: true, Model: "trained", Positives: 9, Negatives: 3 }),
        phrase({ Slug: "hey-clanka", Selected: true, Missing: ["no positive samples", "no trained model"], NeedsAttention: true, StatusLabel: "Needs attention: no positive samples, no trained model", CanRemove: true, RemoveAction: "/voice-samples/phrases/hey-clanka/remove" }),
      ])}
      pending=""
      onRun={vi.fn()}
      onSelect={vi.fn()}
    />,
  );
  const flagged = screen.getByRole("button", { name: /hey clanka\. Needs attention: no positive samples, no trained model/ });
  expect(flagged.textContent).toContain("!");
  expect(flagged.querySelector("[title]")?.getAttribute("title")).toBe("Needs attention: no positive samples, no trained model");
  expect(screen.getByRole("button", { name: /^clanker\./ }).textContent).not.toContain("!");
  expect(screen.getByRole("status").textContent).toContain("Needs: no positive samples, no trained model.");
});

it("adds a phrase, shows the normalised name, and removes an empty one", async () => {
  const run = vi.fn().mockResolvedValue(true);
  render(
    <PhrasePicker
      data={view([phrase({ Slug: "good-night", Selected: true, CanRemove: true, RemoveAction: "/voice-samples/phrases/good-night/remove" })])}
      pending=""
      onRun={run}
      onSelect={vi.fn()}
    />,
  );
  fireEvent.change(screen.getByLabelText("New phrase"), { target: { value: "Hey Clanka" } });
  expect(screen.getByText("hey-clanka")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Add phrase/ }));
  expect(run).toHaveBeenCalledWith("/voice-samples/phrases", { phrase: "Hey Clanka", current: "good-night" });

  fireEvent.click(screen.getByRole("button", { name: /Remove phrase/ }));
  expect(run).toHaveBeenCalledWith("/voice-samples/phrases/good-night/remove");
});

it("disables removal while the phrase still has takes", () => {
  render(
    <PhrasePicker
      data={view([phrase({ Slug: "good-night", Selected: true, Positives: 2, RemoveAction: "/voice-samples/phrases/good-night/remove" })])}
      pending=""
      onRun={vi.fn()}
      onSelect={vi.fn()}
    />,
  );
  const remove = screen.getByRole("button", { name: /Remove phrase/ }) as HTMLButtonElement;
  expect(remove.disabled || remove.getAttribute("aria-disabled") === "true").toBe(true);
  expect(screen.getByText("Delete its takes to remove it.")).toBeTruthy();
});
