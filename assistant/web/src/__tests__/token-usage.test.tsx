// The Activity detail row shows counts only, and says "unknown" when a row
// carries none (an old row, or a provider that omitted usage).
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { RequestDetail } from "../chat";
import { req } from "./harness";

describe("token usage display", () => {
  it("renders prompt and completion counts as an in/out line", () => {
    render(<RequestDetail r={req({ ID: "r", PromptTokens: 12000, CompletionTokens: 34 })} />);
    expect(screen.getByText("12000 in / 34 out")).toBeTruthy();
  });

  it("renders unknown instead of zero or free when usage is absent", () => {
    render(<RequestDetail r={req({ ID: "old" })} />);
    expect(screen.getByText("unknown")).toBeTruthy();
    expect(screen.queryByText("0 in / 0 out")).toBeNull();
    expect(screen.queryByText(/free/i)).toBeNull();
  });
});
