// Live preview for the Agent wording rows. The draft in the editor is unsaved,
// so this renders what a save would speak or match rather than the stored
// value. Audio is page-level: one element, shared through useWordingPlayer, so
// a second preview cannot leave the first speaking and leaving Settings stops
// anything still playing. The clip is fetched as a blob before playing, so a
// refused preview (blank, too long, or a failed synthesis) is a short line in
// the row instead of a silent media error.
import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Button } from "@heroui/react";
import { Play, Square } from "lucide-react";
import type { Setting } from "./types";

/** One plausible request tail, so a prefix reads as something you would say. */
const sampleTail = "what time is it";

/** The one page-level audio element behind every wording preview. */
export type WordingPlayer = {
  /** The setting whose clip is playing, if any. */
  playingKey: string;
  /** The row the last failure belongs to, so only that row shows it. */
  errorKey: string;
  error: string;
  /** Fetch and play this row's draft; pressing it again stops. */
  play: (key: string, url: string) => Promise<void>;
  /** The draft this clip described changed (or its row closed): drop it. */
  reset: (key: string) => void;
};

export function useWordingPlayer(): WordingPlayer {
  const [playingKey, setPlayingKey] = useState("");
  const [errorKey, setErrorKey] = useState("");
  const [error, setError] = useState("");
  // Refs mirror the state so every callback identity stays stable: the preview
  // rows call reset from an effect keyed on the draft, and a new identity on
  // every render would stop the clip the user just started.
  const playing = useRef("");
  const failed = useRef("");
  const seq = useRef(0);
  const audio = useRef<HTMLAudioElement | null>(null);
  const objectURL = useRef("");

  const release = useCallback(() => {
    audio.current?.pause();
    audio.current = null;
    if (objectURL.current) URL.revokeObjectURL(objectURL.current);
    objectURL.current = "";
  }, []);

  const stop = useCallback(() => {
    seq.current += 1;
    playing.current = "";
    release();
    setPlayingKey("");
  }, [release]);

  const finish = useCallback(
    (key: string) => {
      if (playing.current !== key) return;
      seq.current += 1;
      playing.current = "";
      release();
      setPlayingKey("");
    },
    [release],
  );

  // A clip outlives no visit: unmounting Settings must not keep speaking.
  useEffect(
    () => () => {
      seq.current += 1;
      release();
    },
    [release],
  );

  const reset = useCallback(
    (key: string) => {
      if (playing.current === key) stop();
      if (failed.current === key) {
        failed.current = "";
        setError("");
        setErrorKey("");
      }
    },
    [stop],
  );

  const play = useCallback(
    async (key: string, url: string) => {
      if (playing.current === key) {
        stop();
        return;
      }
      const request = (seq.current += 1);
      release();
      playing.current = key;
      failed.current = "";
      setPlayingKey(key);
      setError("");
      setErrorKey("");
      try {
        let res: Response;
        try {
          res = await fetch(url, { credentials: "same-origin" });
        } catch {
          throw new Error("Could not reach the assistant for a preview.");
        }
        // A newer play (or a changed draft) owns the page now; this answer is
        // for words nobody is looking at any more.
        if (request !== seq.current) return;
        if (!res.ok) throw new Error((await res.text()).trim() || "Wording preview unavailable.");
        const blobURL = URL.createObjectURL(await res.blob());
        if (request !== seq.current) {
          URL.revokeObjectURL(blobURL);
          return;
        }
        objectURL.current = blobURL;
        const clip = new Audio(blobURL);
        audio.current = clip;
        clip.onended = clip.onerror = () => finish(key);
        await clip.play();
      } catch (e) {
        if (request !== seq.current) return;
        release();
        playing.current = "";
        setPlayingKey("");
        failed.current = key;
        setErrorKey(key);
        setError(e instanceof Error && e.message ? e.message : "Wording preview unavailable.");
      }
    },
    [finish, release, stop],
  );

  return { playingKey, errorKey, error, play, reset };
}

/**
 * The preview block for one Agent wording row. `draft` is the editor's own
 * state, so typing updates the line before anything is saved.
 */
export function WordingPreview({
  s, draft, voice, canPreview, maxRunes, player,
}: {
  s: Setting;
  draft: string;
  voice: string;
  canPreview: boolean;
  maxRunes: number;
  player: WordingPlayer;
}) {
  const { reset, play } = player;
  // The label is in the button name so two open rows stay distinguishable in a
  // screen reader's button list; the ids describe why Play is disabled.
  const label = s.Label;
  const blankNoteID = useId();
  const limitNoteID = useId();
  // New words make the old clip wrong: stop it rather than letting a stale
  // sentence speak over the draft on screen.
  useEffect(() => {
    reset(s.Key);
  }, [draft, reset, s.Key]);
  // A closed row unmounts its preview; its clip must not keep going.
  useEffect(() => () => reset(s.Key), [reset, s.Key]);

  if (s.PreviewKind === "match") return <MatchPreview s={s} draft={draft} />;
  if (s.PreviewKind !== "spoken") return null;

  const text = draft.trim();
  const blank = text === "";
  const count = [...text].length;
  const over = maxRunes > 0 && count > maxRunes;
  const playing = player.playingKey === s.Key;
  const failure = player.errorKey === s.Key ? player.error : "";
  const audio = canPreview && s.PreviewAction !== "";
  const sayable = audio && !blank && !over;
  const describedBy = blank ? blankNoteID : over ? limitNoteID : undefined;

  return (
    <div role="group" aria-label={`Wording preview for ${s.Label}`} className="border bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b px-2.5 py-1.5">
        <span className="font-mono text-[10px] tracking-[0.12em] text-muted uppercase">{audio && voice ? `Preview · ${voice}` : "Preview"}</span>
        {audio && (
          <Button
            type="button"
            size="sm"
            variant="secondary"
            aria-label={`${playing ? "Stop" : "Play"} the preview of ${label}`}
            aria-describedby={describedBy}
            isDisabled={!sayable && !playing}
            onPress={() => void play(s.Key, `${s.PreviewAction}?text=${encodeURIComponent(draft)}`)}
          >
            {playing ? <Square className="size-3.5 fill-current" /> : <Play className="size-3.5" />}
            {playing ? "Stop" : "Play"}
          </Button>
        )}
      </div>
      <div className="space-y-1.5 px-2.5 py-2">
        {!blank && <p className="font-serif text-base leading-snug break-words">{text}</p>}
        {blank && (
          <p id={blankNoteID} className="text-xs text-muted">
            A blank draft can't be saved, so there's nothing to preview.
          </p>
        )}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-subtle">
          {maxRunes > 0 && (
            <span className={over ? "text-danger" : undefined}>
              {count} / {maxRunes} characters
            </span>
          )}
          {over && (
            <span id={limitNoteID} className="text-danger">
              Too long for a preview; shorten the wording.
            </span>
          )}
          {!canPreview && <span>Preview needs the speech service.</span>}
        </div>
        {failure && (
          <p role="alert" className="text-xs break-words text-danger">
            {failure}
          </p>
        )}
      </div>
    </div>
  );
}

/** Match wording is checked in what the user says, so it has no Play button. */
function MatchPreview({ s, draft }: { s: Setting; draft: string }) {
  const samples = prefixes(draft);
  return (
    <div role="group" aria-label={`Wording preview for ${s.Label}`} className="border bg-surface">
      <div className="border-b px-2.5 py-1.5">
        <span className="font-mono text-[10px] tracking-[0.12em] text-muted uppercase">Preview · matched when you speak</span>
      </div>
      <div className="px-2.5 py-2">
        {samples.length ? (
          <ul className="space-y-1">
            {samples.map((sample) => (
              <li key={sample} className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5">
                <span className="min-w-0 font-serif text-sm break-words">{utterance(sample)}</span>
                <span className="font-mono text-[10px] text-muted">→ Agent path</span>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-xs text-muted">No prefixes; nothing routes to the agent path by prefix.</p>
        )}
        <p className="mt-1.5 text-[11px] text-subtle">
          Checked against what you say, never spoken. Capitals, spacing and closing punctuation don't matter; the longest prefix wins.
        </p>
      </div>
    </div>
  );
}

/** Mirrors internal/phrase.Normalize: case, spacing and a final ?.! do not count. */
function normalizePrefix(text: string): string {
  return text.trim().replace(/[?.!]+$/, "").split(/\s+/).join(" ").toLowerCase();
}

/** One normalised entry per non-blank line, in the order it was typed. */
function prefixes(draft: string): string[] {
  const seen = new Set<string>();
  for (const line of draft.split("\n")) {
    const prefix = normalizePrefix(line);
    if (prefix) seen.add(prefix);
  }
  return [...seen];
}

/** The sample request: a comma unless the prefix already ends in one. */
function utterance(prefix: string): string {
  return /[,;:]$/.test(prefix) ? `${prefix} ${sampleTail}` : `${prefix}, ${sampleTail}`;
}
