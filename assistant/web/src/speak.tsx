// The opt-in that lets a chat answer speak on the Pi. The server owns the
// setting; this context only mirrors it so the top-bar toggle and the
// composer's "Spoken on the Pi / Shown only" hint agree.
import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { Button } from "@heroui/react";
import { Megaphone, MegaphoneOff } from "lucide-react";
import { postForm } from "./api";
import { useSession } from "./shell";
import { cn, Tip } from "./ui";

export type SpeakState = {
  /** A speaker is wired; without one the toggle is hidden. */
  can: boolean;
  on: boolean;
  error: string;
  toggle: () => void;
};

const SpeakCtx = createContext<SpeakState>({ can: false, on: false, error: "", toggle: () => {} });
export const useSpeak = () => useContext(SpeakCtx);

export function SpeakProvider({ children }: { children: ReactNode }) {
  const session = useSession();
  const [on, setOn] = useState(session.Speak);
  const [error, setError] = useState("");

  useEffect(() => setOn(session.Speak), [session.Speak]);

  const toggle = useCallback(() => {
    const next = !on;
    setOn(next);
    setError("");
    // The write answers with the state the server saved, so a refusal puts the
    // toggle back where it was instead of claiming a change that did not land.
    postForm<{ Speak: boolean }>("/api/ui/speak", { value: String(next) }).then(
      (res) => {
        if (res.view) setOn(res.view.Speak);
        if (!res.ok) {
          setOn(!next);
          setError(res.text || "The setting was not saved.");
        }
      },
      () => {
        setOn(!next);
        setError("The setting was not saved.");
      },
    );
  }, [on]);

  return <SpeakCtx.Provider value={{ can: session.CanSpeak, on, error, toggle }}>{children}</SpeakCtx.Provider>;
}

/** Top-bar speaker: whether an answer will be heard or only shown. */
export function SpeakToggle() {
  const { can, on, error, toggle } = useSpeak();
  if (!can) return null;
  const Icon = on ? Megaphone : MegaphoneOff;
  return (
    <div className="flex items-center gap-1">
      <Tip hint={on ? "On: answers also play on the Pi speaker." : "Off: answers are only shown here."}>
        <Button
          isIconOnly
          variant="ghost"
          onPress={toggle}
          aria-label="Speak replies on the Pi"
          aria-pressed={on}
          aria-description={on ? "Answers are spoken on the Pi" : "Answers are shown only"}
        >
          <Icon className={cn("size-5", on ? "text-foreground" : "text-muted")} strokeWidth={1.5} />
        </Button>
      </Tip>
      {error && (
        <span role="status" className="max-w-40 truncate text-[11px] text-danger">
          {error}
        </span>
      )}
    </div>
  );
}

/** The composer's one-line promise about the answer. */
export function SpeakHint() {
  const { can, on } = useSpeak();
  if (!can) return null;
  return (
    <span className="flex items-center gap-1 text-[11px] text-subtle" title={on ? "The answer will also play on the Pi speaker" : "The answer will only be shown here"}>
      {on ? <Megaphone className="size-3" /> : <MegaphoneOff className="size-3" />}
      {on ? "Spoken on the Pi" : "Shown only"}
    </span>
  );
}
