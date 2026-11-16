// Browser playback for one chat reply. Audio is page-level: one element shared
// through useReplyAudio, so a second Play stops the first, a second press on the
// same row stops it, and leaving the conversation stops anything still speaking.
// The clip is fetched as a blob before playing, so a refused reply (blank, too
// long, unknown id, or a failed synthesis) is a short line under that exchange
// instead of a silent media error. Nothing here plays on the Pi, and nothing
// autoplays: sound starts only from the button press.
//
// The URL carries only the request id; the reply text stays on the server. A
// Local-only reply is already visible on this authenticated page, so hearing it
// in the owner's own browser does not widen what leaves the Pi.
import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { Button } from "@heroui/react";
import { Play, Square } from "lucide-react";
import { useSession } from "./shell";

/** The one page-level audio element behind every reply's Play button. */
export type ReplyPlayer = {
  /** The request whose clip is playing, if any. */
  playingID: string;
  /** The exchange the last failure belongs to, so only that row shows it. */
  errorID: string;
  error: string;
  /** Fetch and play this reply's clip; pressing its own row again stops it. */
  play: (id: string, url: string) => Promise<void>;
  /** Drop a stale clip or failure when its exchange goes away. */
  reset: (id: string) => void;
};

const ReplyAudioCtx = createContext<ReplyPlayer | null>(null);
export const useReplyAudio = () => useContext(ReplyAudioCtx);

export function ReplyAudioProvider({ children }: { children: ReactNode }) {
  const [playingID, setPlayingID] = useState("");
  const [errorID, setErrorID] = useState("");
  const [error, setError] = useState("");
  // Refs mirror the state so callback identities stay stable: a row calls reset
  // from an effect keyed on its id, and a fresh identity would stop the clip the
  // reader just started on every render.
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
    setPlayingID("");
  }, [release]);

  const finish = useCallback(
    (id: string) => {
      if (playing.current !== id) return;
      seq.current += 1;
      playing.current = "";
      release();
      setPlayingID("");
    },
    [release],
  );

  // A clip outlives no visit: navigation unmounts Home and must not keep
  // speaking into another page.
  useEffect(
    () => () => {
      seq.current += 1;
      release();
    },
    [release],
  );

  const reset = useCallback(
    (id: string) => {
      if (playing.current === id) stop();
      if (failed.current === id) {
        failed.current = "";
        setError("");
        setErrorID("");
      }
    },
    [stop],
  );

  const play = useCallback(
    async (id: string, url: string) => {
      if (playing.current === id) {
        stop();
        return;
      }
      const request = (seq.current += 1);
      release();
      playing.current = id;
      failed.current = "";
      setPlayingID(id);
      setError("");
      setErrorID("");
      try {
        let res: Response;
        try {
          res = await fetch(url, { credentials: "same-origin" });
        } catch {
          throw new Error("Could not reach the assistant for reply audio.");
        }
        // A newer play (or a row that went away) owns the page now; this clip
        // is for a reply nobody is looking at any more.
        if (request !== seq.current) return;
        if (!res.ok) throw new Error((await res.text()).trim() || "Reply audio unavailable.");
        const blobURL = URL.createObjectURL(await res.blob());
        if (request !== seq.current) {
          URL.revokeObjectURL(blobURL);
          return;
        }
        objectURL.current = blobURL;
        const clip = new Audio(blobURL);
        audio.current = clip;
        clip.onended = clip.onerror = () => finish(id);
        await clip.play();
      } catch (e) {
        if (request !== seq.current) return;
        release();
        playing.current = "";
        setPlayingID("");
        failed.current = id;
        setErrorID(id);
        setError(e instanceof Error && e.message ? e.message : "Reply audio unavailable.");
      }
    },
    [finish, release, stop],
  );

  return <ReplyAudioCtx.Provider value={{ playingID, errorID, error, play, reset }}>{children}</ReplyAudioCtx.Provider>;
}

/**
 * The per-exchange control. It is absent when no reply text exists or the
 * deployment cannot synthesize (no TTS/Activity service), rather than offering
 * a dead button or quietly routing the answer to the Pi speaker.
 */
export function PlayReply({ id, reply, time }: { id: string; reply: string; time: string }) {
  const player = useReplyAudio();
  const { CanPlayReplies } = useSession();
  const reset = player?.reset;
  // A row that unmounts (view switch, navigation, history trim) must not leave
  // its clip running. `reset` is stable, so this does not fire on re-render.
  useEffect(() => () => reset?.(id), [reset, id]);

  if (!player || !CanPlayReplies || !reply.trim()) return null;
  const playing = player.playingID === id;
  const failure = player.errorID === id ? player.error : "";
  // The time keeps two open rows distinguishable in a screen reader's button
  // list; the visible label stays short and matches the row's wording preview.
  const label = `${playing ? "Stop playing" : "Play"} reply aloud${time ? ` from ${time}` : ""}`;
  return (
    <div className="space-y-1">
      <Button
        type="button"
        size="sm"
        variant="ghost"
        aria-label={label}
        onPress={() => void player.play(id, `/reply/audio/${encodeURIComponent(id)}`)}
      >
        {playing ? <Square className="size-3 fill-current" /> : <Play className="size-3" />}
        {playing ? "Stop" : "Play aloud"}
      </Button>
      {failure && (
        <p role="alert" className="text-xs break-words text-danger">
          {failure}
        </p>
      )}
    </div>
  );
}
