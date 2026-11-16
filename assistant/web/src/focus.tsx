// The "what holds Focus / what is waiting" strip shown in the Activity header.
// It reports only counts and flags, never announcement text: a Reminder message
// is private content, so the Dashboard must not surface it (#217).
import type { FocusView } from "./types";
import { cn } from "./ui";

export function emptyFocus(): FocusView {
  return {
    ring: false,
    reply: false,
    music: false,
    announcements: { waiting: 0, held: false, playing: false },
  };
}

export function FocusStatus({ view }: { view: FocusView }) {
  const holders: string[] = [];
  if (view.ring) holders.push("Ringing");
  if (view.reply) holders.push("Reply");
  if (view.music) holders.push("Music");
  // Nothing holds Focus: the assistant is idle and listening.
  if (holders.length === 0) holders.push("Listening");

  const announce: string[] = [];
  const waiting = view.announcements.waiting;
  if (waiting > 0) announce.push(`${waiting} announcement${waiting === 1 ? "" : "s"} waiting`);
  if (view.announcements.held) announce.push("held");
  if (view.announcements.playing) announce.push("playing");

  return (
    <div role="status" aria-live="polite" className="flex flex-wrap items-center gap-2 text-xs text-muted">
      {holders.map((h) => (
        <span
          key={h}
          className="inline-flex items-center gap-1 rounded-full border border-separator px-2 py-0.5"
        >
          <span
            className={cn("size-1.5 rounded-full", h === "Listening" ? "bg-muted" : "bg-foreground")}
            aria-hidden="true"
          />
          {h}
        </span>
      ))}
      {announce.map((a) => (
        <span key={a} className="inline-flex items-center gap-1 rounded-full border border-separator px-2 py-0.5">
          {a}
        </span>
      ))}
    </div>
  );
}
