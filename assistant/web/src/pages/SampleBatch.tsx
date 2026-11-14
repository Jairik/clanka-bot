// A separate batch flow leaves the existing single-take and chat recorders untouched.
import { useEffect, useRef, useState } from "react";
import { Button, Input } from "@heroui/react";
import { micSupported, startRecording, type Recording } from "../mic";
import { encodeWAV } from "../wake-sample";
import { assessSample } from "../sample-quality";
import type { VoiceSamplesView } from "../types";
import { Lede, Section } from "../ui";

type Take = ReturnType<typeof assessSample> & { url: string; saved: boolean; prompt: string };
const wait = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));
const clankerNegatives = ["clank", "clunk", "clinker", "clan car", "what time is it"];
const ordinaryNegatives = ["what time is it", "turn on the kitchen light", "play some music", "stop the music", "good morning"];

// A short quiet browser cue is used only when capturing this device's microphone.
async function beep(ctx: AudioContext) {
  await ctx.resume();
  const tone = ctx.createOscillator();
  const gain = ctx.createGain();
  tone.frequency.value = 880;
  gain.gain.setValueAtTime(0, ctx.currentTime);
  gain.gain.linearRampToValueAtTime(0.02, ctx.currentTime + 0.01);
  gain.gain.linearRampToValueAtTime(0, ctx.currentTime + 0.06);
  tone.connect(gain).connect(ctx.destination);
  tone.start();
  tone.stop(ctx.currentTime + 0.07);
  await wait(80);
}

export function SampleBatch({ data, kind, onSaved, onBusy, onDirty }: {
  data: VoiceSamplesView;
  kind: "positive" | "negative";
  onSaved: () => Promise<void>;
  onBusy: (busy: boolean) => void;
  onDirty: (dirty: boolean) => void;
}) {
  const [source, setSource] = useState("pi");
  const [count, setCount] = useState(kind === "positive" ? "10" : "5");
  const [takes, setTakes] = useState<(Take | null)[]>([]);
  const [busy, setBusy] = useState(false);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [current, setCurrent] = useState("");
  const runID = useRef(0);
  const active = useRef<Recording | null>(null);
  const request = useRef<AbortController | null>(null);
  const cue = useRef<AudioContext | null>(null);
  const rows = useRef<(Take | null)[]>([]);
  const mounted = useRef(true);
  const player = useRef<HTMLAudioElement | null>(null);
  useEffect(() => {
    const warn = (event: BeforeUnloadEvent) => {
      if (rows.current.some((take) => take && !take.saved)) { event.preventDefault(); event.returnValue = ""; }
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, []);
  const negatives = data.Slug === "clanker" ? clankerNegatives : ordinaryNegatives;
  const upload = kind === "positive" ? data.UploadPositive : data.UploadNegative;
  const limit = data.MaxTakes - (kind === "positive" ? data.PositiveCount : data.NegativeCount);
  const total = Number(count);
  const validCount = Number.isInteger(total) && total > 0 && total <= limit;
  const update = (next: (Take | null)[]) => { rows.current = next; setTakes(next); onDirty(next.some((take) => !!take && !take.saved)); };

  const pause = () => {
    runID.current++;
    request.current?.abort();
    active.current?.cancel();
    active.current = null;
    player.current?.pause();
    setStatus("Paused. The interrupted take will be retried.");
  };
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      runID.current++;
      request.current?.abort();
      active.current?.cancel();
      player.current?.pause();
      void cue.current?.close();
      rows.current.forEach((take) => take && URL.revokeObjectURL(take.url));
    };
  }, []);

  const run = async (redo?: number) => {
    if (busy || saving || (!takes.length && !validCount)) return;
    const id = ++runID.current;
    setBusy(true);
    onBusy(true);
    setError("");
    player.current?.pause();
    if (!rows.current.length) update(Array.from({ length: total }, () => null));
    try {
      if (source === "browser") {
        cue.current ??= new AudioContext();
        await cue.current.resume();
      }
      const indices = redo === undefined ? rows.current.flatMap((take, i) => take ? [] : [i]) : [redo];
      for (const i of indices) {
        if (id !== runID.current) break;
        const prompt = kind === "positive" ? `Say “${data.Phrase}” naturally at your normal distance.` : `Say “${negatives[i % negatives.length]}”.`;
        setCurrent(prompt);
        setStatus(`Take ${i + 1} of ${rows.current.length}: wait for the quiet beep, then speak. Each capture lasts 3 seconds.`);
        // Give the owner time to read a changed prompt before the cue.
        await wait(700);
        if (id !== runID.current) break;
        let pcm: ArrayBuffer;
        if (source === "pi") {
          const controller = new AbortController();
          request.current = controller;
          const response = await fetch("/voice-samples/capture", { method: "POST", credentials: "same-origin", signal: controller.signal });
          if (!response.ok) throw new Error((await response.text()).trim());
          pcm = await response.arrayBuffer();
        } else {
          const recording = await startRecording(() => {}, { maxSeconds: 3, beforeCapture: async () => {
            if (id !== runID.current) throw new DOMException("Aborted", "AbortError");
            await beep(cue.current!);
          }, processing: { echoCancellation: false, noiseSuppression: false, autoGainControl: false } });
          if (id !== runID.current) { recording.cancel(); break; }
          active.current = recording;
          await wait(3000);
          if (id !== runID.current) break;
          pcm = await recording.stop();
          active.current = null;
        }
        if (id !== runID.current) break;
        const result = assessSample(pcm);
        const take = { ...result, url: URL.createObjectURL(new Blob([encodeWAV(new Int16Array(pcm))], { type: "audio/wav" })), saved: false, prompt };
        const next = [...rows.current];
        if (next[i]) URL.revokeObjectURL(next[i]!.url);
        next[i] = take;
        update(next);
      }
      if (id === runID.current) setStatus("Batch ready for review. Redo recommended takes, then save the batch to the Pi.");
    } catch (e) {
      if (id === runID.current) setError(e instanceof Error ? e.message : "Recording failed.");
    } finally {
      if (mounted.current) { setBusy(false); onBusy(false); }
    }
  };

  const save = async () => {
    setSaving(true);
    onBusy(true);
    setError("");
    try {
      for (let i = 0; i < rows.current.length; i++) {
        const take = rows.current[i];
        if (!take || take.saved || take.blocked) continue;
        if (!mounted.current) break;
        const controller = new AbortController();
        request.current = controller;
        const response = await fetch(upload, { method: "POST", credentials: "same-origin", headers: { "Content-Type": "audio/wav", Accept: "application/json" }, body: take.wav, signal: controller.signal });
        if (!response.ok) {
          const view = await response.json().catch(() => null);
          throw new Error(view?.Error || "Saving failed. Already saved takes are preserved; retry the rest.");
        }
        const next = [...rows.current];
        next[i] = { ...take, saved: true };
        update(next);
      }
      await onSaved();
      setStatus("Saved to the Pi. Submit the set below when both batches are complete.");
    } catch (e) { setError(e instanceof Error ? e.message : "Saving failed."); }
    finally { setSaving(false); onBusy(false); }
  };

  const locked = busy || saving || takes.length > 0;
  return <Section title={`Batch: ${kind === "positive" ? "positives" : "negatives"}`} defaultOpen>
    <Lede>Record a batch without confirming each take. A quiet beep on the selected device’s speaker precedes each three-second capture. Acoustic checks recommend retakes; they do not verify the words. Replay uncertain takes and keep complete phrases.</Lede>
    <div className="space-y-3 px-3.5 pb-4">
      <label className="flex items-center gap-2 text-sm">Microphone
        <select aria-label="Batch microphone" value={source} disabled={locked} onChange={(e) => setSource(e.target.value)}>
          <option value="pi">Pi microphone (configured USB source)</option><option value="browser">This device (browser microphone)</option>
        </select>
      </label>
      {source === "pi" && !data.PiRecordingAvailable && <p className="text-xs text-warning">Pi capture is unavailable on this deployment. No browser microphone will be used instead.</p>}
      <label className="flex items-center gap-2 text-sm">New takes<Input aria-label="Batch take count" type="number" min={1} max={limit} value={count} disabled={locked} onChange={(e) => setCount(e.target.value)} className="max-w-24" /></label>
      <div className="flex flex-wrap gap-2">
        <Button onPress={() => void run()} isDisabled={busy || saving || (!takes.length && !validCount) || (takes.length > 0 && takes.every(Boolean)) || (source === "pi" ? !data.PiRecordingAvailable : !micSupported())}>{takes.length ? "Continue batch" : "Start batch"}</Button>
        {busy && <Button variant="secondary" onPress={pause}>Pause / stop take</Button>}
        {!!takes.length && <Button variant="secondary" onPress={() => void save()} isDisabled={busy || saving || takes.some((take) => !take || take.blocked) || takes.every((take) => take?.saved)} isPending={saving}>Save batch to Pi</Button>}
        {!!takes.length && <Button variant="ghost" isDisabled={busy || saving} onPress={() => { rows.current.forEach((take) => take && URL.revokeObjectURL(take.url)); update([]); setStatus(""); }}>New batch / clear review</Button>}
      </div>
      <p role="status" className="text-sm">{status}</p>
      {busy && <p className="text-sm font-medium">{current}</p>}
      {error && <p role="alert" className="text-xs text-danger">{error}</p>}
      <p className="text-xs text-muted">Unsaved takes stay in this page’s memory. Save before leaving or switching tabs; phrase and take-kind switching stays locked while there are unsaved takes. Missing, boundary-cut and truncated takes must be redone before saving.</p>
      <ul className="space-y-2">{takes.map((take, i) => <li key={i} className="flex flex-wrap items-center gap-2 border-t pt-2 text-xs">
        <span>Take {i + 1}: {take ? take.saved ? "Saved" : take.warnings.length ? "Redo recommended" : "No acoustic issues found" : "Pending"}</span>
        {take && <><span>{take.seconds.toFixed(2)} s · {take.prompt}</span><span className="text-warning">{take.warnings.join(" ")}</span>
          <Button size="sm" variant="secondary" isDisabled={busy || saving} onPress={() => { player.current?.pause(); player.current = new Audio(take.url); void player.current.play().catch(() => setError("Playback failed.")); }}>Play take {i + 1}</Button>
        </>}
        <Button size="sm" variant="ghost" isDisabled={busy || saving || !!take?.saved} onPress={() => void run(i)}>Redo take {i + 1}</Button>
      </li>)}</ul>
    </div>
  </Section>;
}
