// The composer's microphone state machine. One recording at a time: a second
// start while the browser's permission prompt is still open is ignored,
// otherwise the second capture replaces the first and the first stream is
// never stopped. A recording ends by itself at the cap.
import { useCallback, useEffect, useRef, useState } from "react";
import { MIC_MAX_SECONDS, startRecording, type Recording } from "./mic";

export type MicPhase = "idle" | "starting" | "recording" | "error";

export type Recorder = {
  phase: MicPhase;
  /** Whole seconds recorded so far; 0 while idle. */
  elapsed: number;
  levels: number[];
  error: string;
  start: () => void;
  /** Stop and upload the utterance. */
  stop: () => void;
  /** Stop and throw the utterance away. */
  cancel: () => void;
};

const idleLevels = () => Array(28).fill(0.06) as number[];
const LEVEL_MS = 50;

/** The server's wording for a tap that never became an utterance. */
export const TOO_SHORT_ERROR = "That recording was too short. Hold the mic and speak.";

export function useRecorder({
  onSend,
  onSent,
  maxSeconds = MIC_MAX_SECONDS,
}: {
  onSend: (pcm: ArrayBuffer) => void;
  onSent?: () => void;
  maxSeconds?: number;
}): Recorder {
  const [phase, setPhase] = useState<MicPhase>("idle");
  const [startedAt, setStartedAt] = useState(0);
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [levels, setLevels] = useState<number[]>(idleLevels);
  const [error, setError] = useState("");
  const recording = useRef<Recording | null>(null);
  const starting = useRef(false);
  // A release can arrive while the browser's permission prompt is still open;
  // remember what it wanted and settle as soon as the capture exists.
  const requested = useRef<"stop" | "cancel" | null>(null);

  const finish = useCallback(
    async (submit: boolean) => {
      if (starting.current) {
        requested.current = submit ? "stop" : "cancel";
        return;
      }
      const r = recording.current;
      recording.current = null;
      setPhase("idle");
      setLevels(idleLevels());
      if (!r) return;
      if (!submit) return r.cancel();
      const pcm = await r.stop();
      onSend(pcm);
      onSent?.();
    },
    [onSend, onSent],
  );

  const start = useCallback(() => {
    if (recording.current || starting.current) return;
    starting.current = true;
    setError("");
    setPhase("starting");
    void (async () => {
      try {
        let last = 0;
        const capture = await startRecording((rms) => {
          const t = performance.now();
          if (t - last < LEVEL_MS) return;
          last = t;
          setLevels((l) => [...l.slice(1), Math.max(0.06, Math.min(1, rms * 6))]);
        });
        // A press that ended before the capture existed never became audio.
        if (requested.current) {
          const intent = requested.current;
          requested.current = null;
          capture.cancel();
          setPhase("idle");
          setLevels(idleLevels());
          if (intent === "stop") setError(TOO_SHORT_ERROR);
          return;
        }
        recording.current = capture;
        setStartedAt(Date.now());
        setNowMs(Date.now());
        setPhase("recording");
      } catch (e) {
        setError(e instanceof DOMException && e.name === "NotAllowedError" ? "Microphone access was blocked." : "Could not start the microphone.");
        setPhase("error");
      } finally {
        starting.current = false;
      }
    })();
  }, []);

  // The visible timer and the cap both read this clock.
  useEffect(() => {
    if (phase !== "recording") return;
    const id = setInterval(() => setNowMs(Date.now()), 200);
    return () => clearInterval(id);
  }, [phase]);

  useEffect(() => {
    if (phase === "recording" && nowMs - startedAt >= maxSeconds * 1000) void finish(true);
  }, [phase, nowMs, startedAt, maxSeconds, finish]);

  // Leaving the page mid-recording must release the microphone.
  useEffect(() => () => recording.current?.cancel(), []);

  const elapsed = phase === "recording" ? Math.max(0, Math.floor((nowMs - startedAt) / 1000)) : 0;
  return {
    phase,
    elapsed,
    levels,
    error,
    start,
    stop: () => void finish(true),
    cancel: () => void finish(false),
  };
}
