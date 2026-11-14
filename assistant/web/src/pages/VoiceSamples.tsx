// Personal wake-phrase recorder: one phrase at a time, takes kept on the Pi,
// and a finished set exported as the same bundle the desktop recorder writes.
// Nothing here uploads audio anywhere; the only way off the box is the explicit
// Download or the copy command shown after Submit.
import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Input, Tabs } from "@heroui/react";
import { Check, Clipboard, Download, Mic, Play, Plus, RotateCcw, Square, Trash2, X } from "lucide-react";
import { getJSON, navigate, useLocation } from "../api";
import { useActions } from "../forms";
import { micSupported, startRecording, type Recording } from "../mic";
import { PageFrame } from "../shell";
import { SampleBatch } from "./SampleBatch";
import { trainingClip } from "../wake-sample";
import type { VoiceSampleBundle, VoiceSamplePhrase, VoiceSampleTake, VoiceSamplesView } from "../types";
import { Badge, Empty, Lede, Notice, Section, Tip } from "../ui";

type Kind = "positive" | "negative";

type Draft = {
  kind: Kind;
  wav: ArrayBuffer;
  url: string;
  seconds: number;
  foundSpeech: boolean;
  quiet: boolean;
};

const AUTO_STOP_MS = 2000;
const CAPTURE_SECONDS = 3;

export function VoiceSamplesPage() {
  const { params } = useLocation();
  const phrase = params.get("phrase") ?? "";
  const [data, setData] = useState<VoiceSamplesView | null>(null);
  const [loadError, setLoadError] = useState("");
  // Adding or removing a phrase answers with the view for the phrase it
  // selected (the new one, or the first after a removal); follow it in the
  // URL so a reload stays there.
  const applyView = useCallback(
    (view: VoiceSamplesView) => {
      setData(view);
      if (view.Slug && view.Slug !== phrase) navigate(`/voice-samples?phrase=${encodeURIComponent(view.Slug)}`);
    },
    [phrase],
  );
  const actions = useActions<VoiceSamplesView>(applyView);
  const [kind, setKind] = useState<Kind>("positive");
 const [batchBusy, setBatchBusy] = useState(false);
 const [batchDirty, setBatchDirty] = useState(false);
 const [singleBusy, setSingleBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const query = phrase ? `?phrase=${encodeURIComponent(phrase)}` : "";
      setData(await getJSON<VoiceSamplesView>(`/api/ui/voice-samples${query}`));
      setLoadError("");
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : String(e));
    }
  }, [phrase]);
  useEffect(() => {
    void load();
  }, [load]);

  return (
    <PageFrame id="voice-samples">
      <Notice error={loadError || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {!data && !loadError && <p className="text-sm text-muted">Loading…</p>}
      {data && !data.HasStore && (
        <Section title="The recorder is not configured" defaultOpen>
          <Empty>This assistant was built without the voice-sample store, so there is nothing to record.</Empty>
        </Section>
      )}
      {data?.HasStore && (
        <div className="space-y-2.5">
          {!batchBusy && !batchDirty && !singleBusy && (
            <PhrasePicker
              data={data}
              pending={actions.pending}
              onRun={actions.run}
              onSelect={(slug) => navigate(`/voice-samples?phrase=${encodeURIComponent(slug)}`)}
            />
          )}
          <div className="flex flex-wrap items-center justify-between gap-3 pt-1">
            <Tabs selectedKey={kind} onSelectionChange={(key) => { if (!batchBusy && !batchDirty && !singleBusy) setKind(String(key) as Kind); }} className="w-auto">
              <Tabs.ListContainer>
                <Tabs.List aria-label="Take kind" className="border">
                  <Tabs.Tab isDisabled={batchBusy || batchDirty || singleBusy} id="positive" className="px-4">
                    Positives
                    <Badge className="ml-1.5">{data.PositiveCount}</Badge>
                    <Tabs.Indicator />
                  </Tabs.Tab>
                  <Tabs.Tab isDisabled={batchBusy || batchDirty || singleBusy} id="negative" className="px-4">
                    Negatives
                    <Badge className="ml-1.5">{data.NegativeCount}</Badge>
                    <Tabs.Indicator />
                  </Tabs.Tab>
                </Tabs.List>
              </Tabs.ListContainer>
            </Tabs>
            <span className="font-mono text-xs tabular-nums text-muted">
              {kind === "positive" ? `${data.PositiveCount} / ${data.MaxTakes}` : `${data.NegativeCount} / ${data.MaxTakes}`}
              {kind === "positive" && ` · at least ${data.MinTakes} to submit`}
            </span>
          </div>
          {!singleBusy && <SampleBatch key={`batch-${data.Slug}-${kind}`} data={data} kind={kind} onSaved={load} onBusy={setBatchBusy} onDirty={setBatchDirty} />}
          {!batchBusy && !batchDirty && <Recorder
            key={`${data.Slug}-${kind}`}
            kind={kind}
            onBusy={setSingleBusy}
            data={data}
            onRun={actions.run}
            pending={actions.pending}
          />}
          {kind === "positive" ? (
            <TakeList
              title="Positive takes"
              takes={data.Positives}
              clearAction={data.ClearPositive}
              pending={actions.pending}
              onDelete={(take) => void actions.run(take.DeleteAction)}
              onClearAll={() => void actions.run(data.ClearPositive)}
            />
          ) : (
            <TakeList
              title="Negative takes"
              takes={data.Negatives}
              clearAction={data.ClearNegative}
              pending={actions.pending}
              onDelete={(take) => void actions.run(take.DeleteAction)}
              onClearAll={() => void actions.run(data.ClearNegative)}
            />
          )}
          <SubmitPanel data={data} pending={actions.pending} onSubmit={() => void actions.run(data.SubmitAction)} />
          <BundleList bundles={data.Bundles} />
        </div>
      )}
    </PageFrame>
  );
}

// ---------- phrase picker ----------

export function PhrasePicker({
  data,
  pending,
  onRun,
  onSelect,
}: {
  data: VoiceSamplesView;
  pending: string;
  onRun: ReturnType<typeof useActions<VoiceSamplesView>>["run"];
  onSelect: (slug: string) => void;
}) {
  const [draft, setDraft] = useState("");
  const phrases = data.Phrases ?? [];
  const selected = phrases.find((p) => p.Selected);
  const preview = slugify(draft);
  const add = async () => {
    if (!draft.trim()) return;
    // The server normalises and checks the name; the preview below is only a hint.
    const ok = await onRun(data.AddPhraseAction, { phrase: draft, current: data.Slug });
    if (ok) setDraft("");
  };
  return (
    <Section title="Phrase" summary={data.Phrase} defaultOpen>
      <Lede>
        Record takes of the phrase this detector should hear. Each phrase is its own sample set and bundle name; the desktop CLI reads the phrase from the bundle. A{" "}
        <Mark /> means the phrase still needs samples or a trained model.
      </Lede>
      <div className="flex flex-wrap gap-2 px-3.5 pb-3">
        {phrases.map((phrase) => (
          <Button
            key={phrase.Slug}
            size="sm"
            variant={phrase.Selected ? "primary" : "secondary"}
            onPress={() => onSelect(phrase.Slug)}
            aria-pressed={phrase.Selected}
            aria-label={`${phrase.Text}. ${phrase.StatusLabel}`}
          >
            {phrase.Text}
            {/* The tooltip is a native title on the mark: a Tooltip trigger would
                wrap this button in a second button role. */}
            {phrase.NeedsAttention && <Mark title={phrase.StatusLabel} />}
          </Button>
        ))}
      </div>
      {selected && <PhraseStatus phrase={selected} note={data.ModelNote} pending={pending} onRemove={() => void onRun(selected.RemoveAction)} />}
      {data.AddPhraseAction && (
        <form
          className="flex flex-wrap items-center gap-2 border-t px-3.5 py-3"
          onSubmit={(e) => {
            e.preventDefault();
            void add();
          }}
        >
          <Input
            aria-label="New phrase"
            aria-describedby="new-phrase-hint"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="New phrase, e.g. Hey Clanka"
            className="w-full max-w-xs text-sm"
            spellCheck={false}
          />
          <Button size="sm" type="submit" variant="secondary" isDisabled={!draft.trim()} isPending={pending === data.AddPhraseAction}>
            <Plus className="size-3.5" /> Add phrase
          </Button>
          <span id="new-phrase-hint" className="text-[11px] text-subtle">
            {draft.trim() ? (
              preview ? (
                <>
                  Saved as <span className="font-mono text-foreground">{preview}</span>, kept even before the first take.
                </>
              ) : (
                "Use one to four words of letters and digits."
              )
            ) : (
              "Up to four words. Saved right away, even with no takes."
            )}
          </span>
        </form>
      )}
    </Section>
  );
}

/** The monochrome "needs attention" mark; the spoken label lives on its button. */
function Mark({ title }: { title?: string }) {
  return (
    <span aria-hidden title={title} className="inline-flex size-4 items-center justify-center border border-current font-mono text-[10px] font-semibold leading-none">
      !
    </span>
  );
}

function PhraseStatus({
  phrase,
  note,
  pending,
  onRemove,
}: {
  phrase: VoiceSamplePhrase;
  note: string;
  pending: string;
  onRemove: () => void;
}) {
  const missing = phrase.Missing ?? [];
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t px-3.5 py-2.5 text-xs">
      <span className="flex items-center gap-1.5" role="status">
        {missing.length ? (
          <>
            <Mark />
            <span>Needs: {missing.join(", ")}.</span>
          </>
        ) : (
          <>
            <Check className="size-3.5" aria-hidden />
            <span>{phrase.Model === "trained" ? "Samples complete; trained model served." : "Samples complete."}</span>
          </>
        )}
      </span>
      <span className="font-mono text-[11px] tabular-nums text-muted">
        {phrase.Positives} + / {phrase.Negatives} − · model {phrase.Model}
      </span>
      {phrase.RemoveAction && (
        <span className="ml-auto flex items-center gap-2">
          {!phrase.CanRemove && <span className="text-[11px] text-subtle">Delete its takes to remove it.</span>}
          <Button size="sm" variant="ghost" className="text-muted" isDisabled={!phrase.CanRemove} isPending={pending === phrase.RemoveAction} onPress={onRemove}>
            <Trash2 className="size-3.5" /> Remove phrase
          </Button>
        </span>
      )}
      {note && <p className="w-full text-[11px] text-subtle">{note}</p>}
    </div>
  );
}

// Mirrors wakesample.SlugFor for the live hint only; the server decides.
function slugify(value: string): string {
  if (/[/\\<>:"|?*.]/.test(value)) return "";
  const words = value
    .trim()
    .toLowerCase()
    .replace(/['_]/g, " ")
    .replace(/[^a-z0-9\s-]/g, "")
    .split(/[\s-]+/)
    .filter(Boolean);
  if (!words.length || words.length > 4) return "";
  const slug = words.join("-");
  return slug.length <= 40 && /[a-z]/.test(slug) ? slug : "";
}

// ---------- recorder ----------

function Recorder({
  kind,
  data,
  onRun,
  pending,
  onBusy,
}: {
  onBusy: (busy: boolean) => void;
  kind: Kind;
  data: VoiceSamplesView;
  onRun: ReturnType<typeof useActions<VoiceSamplesView>>["run"];
  pending: string;
}) {
  const [draft, setDraft] = useState<Draft | null>(null);
  const [rec, setRec] = useState<{ start: number } | null>(null);
  const [starting, setStarting] = useState(false);
  const [converting, setConverting] = useState(false);
  const [levels, setLevels] = useState<number[]>(() => Array(28).fill(0.06));
  const [now, setNow] = useState(Date.now());
  const [micError, setMicError] = useState("");
  const recorder = useRef<Recording | null>(null);
  const autoStop = useRef<ReturnType<typeof setTimeout>>(undefined);
  const draftURL = useRef("");
  const audio = useRef<HTMLAudioElement | null>(null);
  const mounted = useRef(true);

  useEffect(() => { onBusy(!!rec || starting || converting); }, [rec, starting, converting, onBusy]);

  const upload = kind === "positive" ? data.UploadPositive : data.UploadNegative;
  const prompt = kind === "positive" ? data.NextPrompt : data.NextNegativePrompt;
  const supported = micSupported();

  useEffect(() => {
    if (!rec) return;
    const id = setInterval(() => setNow(Date.now()), 200);
    return () => clearInterval(id);
  }, [rec]);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      autoStop.current && clearTimeout(autoStop.current);
      recorder.current?.cancel();
      audio.current?.pause();
      if (draftURL.current) URL.revokeObjectURL(draftURL.current);
    };
  }, []);

  const discard = useCallback(() => {
    audio.current?.pause();
    if (draftURL.current) URL.revokeObjectURL(draftURL.current);
    draftURL.current = "";
    setDraft(null);
  }, []);

  const finish = useCallback(async () => {
    clearTimeout(autoStop.current);
    const active = recorder.current;
    recorder.current = null;
    setRec(null);
    setLevels(Array(28).fill(0.06));
    if (!active) return;
    setConverting(true);
    try {
      const pcm = await active.stop();
      if (!mounted.current) return;
      const clip = trainingClip(pcm);
      const url = URL.createObjectURL(new Blob([clip.wav], { type: "audio/wav" }));
      if (draftURL.current) URL.revokeObjectURL(draftURL.current);
      draftURL.current = url;
      setDraft({ kind, wav: clip.wav, url, seconds: clip.seconds, foundSpeech: clip.foundSpeech, quiet: clip.peak < 0.02 });
    } catch {
      setMicError("The take could not be converted; try again.");
    } finally {
      setConverting(false);
    }
  }, [kind]);

  const start = useCallback(async () => {
    // getUserMedia shows a permission prompt, so a second tap can arrive
    // before the first resolves; one live recorder is the invariant.
    if (recorder.current || starting || converting) return;
    discard();
    setStarting(true);
    setMicError("");
    try {
      let last = 0;
      const active = await startRecording(
        (rms) => {
          const t = performance.now();
          if (t - last < 50) return;
          last = t;
          setLevels((l) => [...l.slice(1), Math.max(0.06, Math.min(1, rms * 6))]);
        },
        {
          maxSeconds: CAPTURE_SECONDS,
          // Keep the room and the real level: a training take is not a chat
          // utterance, and noise suppression would learn the wrong thing.
          processing: { echoCancellation: false, noiseSuppression: false, autoGainControl: false },
        },
      );
      if (!mounted.current) {
        // The page changed while the permission prompt was open; release the
        // mic rather than leaving a live stream behind.
        active.cancel();
        return;
      }
      recorder.current = active;
      setRec({ start: Date.now() });
      setNow(Date.now());
      autoStop.current = setTimeout(() => void finish(), AUTO_STOP_MS);
    } catch (e) {
      setMicError(
        e instanceof DOMException && e.name === "NotAllowedError"
          ? "Microphone access was blocked. Allow it in the browser and try again."
          : "Could not start the microphone.",
      );
    } finally {
      setStarting(false);
    }
  }, [converting, discard, finish, starting]);

  const keep = async () => {
    if (!draft) return;
    const ok = await onRun(upload, new Blob([draft.wav], { type: "audio/wav" }));
    if (ok) discard();
  };

  const redo = () => {
    const previous = draft;
    discard();
    if (previous) void start();
  };

  const playDraft = () => {
    if (!draft) return;
    audio.current?.pause();
    const player = new Audio(draft.url);
    audio.current = player;
    void player.play();
  };

  return (
    <Section title={kind === "positive" ? "Record a positive take" : "Record a negative take"} summary={prompt} defaultOpen>
      <Lede>
        {kind === "positive"
          ? `Say “${data.Phrase}” the way the prompt asks; it stops after two seconds, or press Stop. Up to ${data.MaxSeconds} is captured, then trimmed to 1.9 s and converted to 16 kHz mono WAV here in the browser.`
          : "Record near-misses and ordinary speech, so the detector learns what is not the phrase. These are hard negatives in training and in the false-accept report."}
      </Lede>
      {!supported && (
        <p className="mx-3.5 mb-3 border border-warning/40 bg-warning/8 px-3 py-2 text-xs text-warning">
          The microphone needs HTTPS (tailscale serve) and a browser with getUserMedia support.
        </p>
      )}
      {micError && (
        <p role="alert" className="mx-3.5 mb-3 border border-danger/40 bg-danger/8 px-3 py-2 text-xs text-danger">
          {micError}
        </p>
      )}

      <div className="border-t px-3.5 py-4">
        {rec ? (
          <div className="flex min-h-14 items-center gap-3">
            <Tip>
              <Button
                isIconOnly
                variant="ghost"
                onPress={() => {
                  clearTimeout(autoStop.current);
                  recorder.current?.cancel();
                  recorder.current = null;
                  setRec(null);
                  setLevels(Array(28).fill(0.06));
                }}
                aria-label="Cancel recording"
              >
                <X className="size-5" />
              </Button>
            </Tip>
            <span className="relative flex size-2.5 shrink-0">
              <span className="absolute inset-0 animate-ping rounded-full bg-danger opacity-60" />
              <span className="relative size-2.5 rounded-full bg-danger" />
            </span>
            <div className="flex h-9 min-w-0 flex-1 items-center gap-[3px] overflow-hidden" aria-hidden>
              {levels.map((l, i) => (
                <span key={i} className="w-1 flex-1 rounded-full bg-foreground/70 transition-[height] duration-75" style={{ height: `${l * 100}%` }} />
              ))}
            </div>
            <span className="w-10 text-right font-mono text-xs tabular-nums text-muted">{Math.floor((now - rec.start) / 1000)}s</span>
            <Button variant="danger" onPress={() => void finish()} isPending={converting}>
              <Square className="size-3.5 fill-current" /> Stop
            </Button>
          </div>
        ) : draft ? (
          <div className="space-y-3">
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" variant="secondary" onPress={playDraft}>
                <Play className="size-3.5" /> Play
              </Button>
              <span className="font-mono text-xs tabular-nums text-muted">{draft.seconds.toFixed(2)} s</span>
              <Button size="sm" onPress={() => void keep()} isPending={pending === upload}>
                <Check className="size-3.5" /> Keep
              </Button>
              <Button size="sm" variant="ghost" onPress={redo} isDisabled={!supported}>
                <RotateCcw className="size-3.5" /> Redo
              </Button>
              <span className="text-[11px] text-subtle">Nothing is stored until Keep.</span>
            </div>
            {!draft.foundSpeech && <p className="text-xs text-warning">No speech stood out from the room noise. Play it back; if it is silent, redo.</p>}
            {draft.foundSpeech && draft.quiet && <p className="text-xs text-warning">Very quiet. Move closer or raise the mic gain, then redo.</p>}
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-3">
            <Button onPress={() => void start()} isPending={starting || converting} isDisabled={!supported}>
              <Mic className="size-4" /> {converting ? "Converting…" : "Record"}
            </Button>
            <span className="text-xs text-muted">{prompt}</span>
          </div>
        )}
      </div>
    </Section>
  );
}

// ---------- takes ----------

function TakeList({
  title,
  takes,
  clearAction,
  pending,
  onDelete,
  onClearAll,
}: {
  title: string;
  takes: VoiceSampleTake[] | null;
  clearAction: string;
  pending: string;
  onDelete: (take: VoiceSampleTake) => void;
  onClearAll: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [playing, setPlaying] = useState("");
  const audio = useRef<HTMLAudioElement | null>(null);
  useEffect(() => () => audio.current?.pause(), []);
  const play = (take: VoiceSampleTake) => {
    audio.current?.pause();
    if (playing === take.ID) return setPlaying("");
    const player = new Audio(take.AudioURL);
    audio.current = player;
    player.onended = player.onerror = () => setPlaying("");
    void player.play().catch(() => setPlaying(""));
    setPlaying(take.ID);
  };
  return (
    <Section title={title} summary={`${takes?.length ?? 0} kept`} flush>
      {takes?.length ? (
        <ul className="divide-y divide-separator">
          {takes.map((take) => (
            <li key={take.ID} className="flex min-h-12 items-center gap-2 px-3.5 py-1.5">
              <Tip>
                <Button isIconOnly size="sm" variant={playing === take.ID ? "primary" : "secondary"} onPress={() => play(take)} aria-label={`Play ${take.ID}`}>
                  <Play className="size-3.5" />
                </Button>
              </Tip>
              <span className="font-mono text-[11px] text-muted">{take.ID}</span>
              <span className="ml-auto font-mono text-[11px] tabular-nums text-subtle">
                {take.Duration} · {take.Size} · {take.Added}
              </span>
              <Tip>
                <Button isIconOnly size="sm" variant="ghost" onPress={() => onDelete(take)} isPending={pending === take.DeleteAction} aria-label={`Delete ${take.ID}`}>
                  <Trash2 className="size-3.5" />
                </Button>
              </Tip>
            </li>
          ))}
        </ul>
      ) : (
        <Empty>No takes yet.</Empty>
      )}
      {!!takes?.length && (
        <div className="flex items-center justify-end gap-2 border-t px-3.5 py-2">
          {confirming ? (
            <>
              <span className="text-xs text-danger">Delete all {takes.length}?</span>
              <Button size="sm" variant="danger" isPending={pending === clearAction} onPress={() => (setConfirming(false), onClearAll())}>
                Delete all
              </Button>
              <Button size="sm" variant="ghost" onPress={() => setConfirming(false)}>
                Cancel
              </Button>
            </>
          ) : (
            <Button size="sm" variant="ghost" className="text-muted" onPress={() => setConfirming(true)}>
              <Trash2 className="size-3.5" /> Delete all
            </Button>
          )}
        </div>
      )}
    </Section>
  );
}

// ---------- submit and bundles ----------

function SubmitPanel({ data, pending, onSubmit }: { data: VoiceSamplesView; pending: string; onSubmit: () => void }) {
  return (
    <Section title="Submit the set" summary={data.CanSubmit ? `${data.PositiveCount} positives ready` : `${data.PositiveCount} / ${data.MinTakes} positives`} defaultOpen>
      <Lede>
        Submit writes one .tar.gz in the same <span className="font-mono">real/positive</span> and <span className="font-mono">real/negative</span> layout the desktop recorder uses. It does not start training, upload anything, or appear in the activity feed.
      </Lede>
      <div className="flex flex-wrap items-center gap-3 px-3.5 pb-4">
        <Button onPress={onSubmit} isDisabled={!data.CanSubmit} isPending={pending === data.SubmitAction}>
          <Download className="size-4" /> Submit {data.PositiveCount} takes
        </Button>
        <span className="text-xs text-muted">
          {data.CanSubmit
            ? `Ready. ${data.NegativeCount} negative take${data.NegativeCount === 1 ? " goes" : "s go"} in the same bundle.`
            : `Keep at least ${data.MinTakes} positive takes first; the limit is ${data.MaxTakes}.`}
        </span>
      </div>
    </Section>
  );
}

function BundleList({ bundles }: { bundles: VoiceSampleBundle[] | null }) {
  if (!bundles?.length) return null;
  return (
    <Section title="Bundles" summary="newest first" defaultOpen>
      <Lede>
        The bundle holds this phrase's takes. Copy it to the desktop, then run <span className="font-mono">wakeword/train import &lt;file&gt;</span> there. The Pi never trains and nothing leaves the box without this step.
      </Lede>
      <ul className="divide-y divide-separator">
        {bundles.map((bundle) => (
          <li key={bundle.Name} className="space-y-2 px-3.5 py-3">
            <div className="flex flex-wrap items-center gap-2">
              <span className="w-full break-all font-mono text-xs sm:w-auto sm:min-w-0 sm:flex-1">{bundle.Name}</span>
              <Badge>{bundle.Positives} + / {bundle.Negatives} −</Badge>
              <span className="text-[11px] text-subtle">{bundle.Size} · {bundle.Created}</span>
              <a
                href={bundle.DownloadURL}
                className="inline-flex h-8 items-center gap-1.5 border px-2.5 text-xs hover:bg-surface-secondary"
                download
              >
                <Download className="size-3.5" /> Download
              </a>
            </div>
            {bundle.SCP && <CopyLine label="scp" value={bundle.SCP} />}
            {bundle.Taildrop && <CopyLine label="Taildrop" value={bundle.Taildrop} />}
            {!bundle.SCP && !bundle.Taildrop && (
              <p className="text-[11px] text-subtle">
                {bundle.HasCopyPath
                  ? `On the Pi: ${bundle.HostPath}. Copy commands are hidden until the SSH user/host and desktop name are configured.`
                  : "Copy commands are hidden until voice_samples.host_dir and the SSH/desktop values are configured; Download works as-is."}
              </p>
            )}
          </li>
        ))}
      </ul>
    </Section>
  );
}

function CopyLine({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const id = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(id);
  }, [copied]);
  return (
    <div className="flex items-center gap-2 border bg-surface-secondary px-2 py-1.5">
      <span className="shrink-0 text-[10px] uppercase tracking-wide text-subtle">{label}</span>
      <code className="min-w-0 flex-1 overflow-x-auto whitespace-pre font-mono text-[11px]">{value}</code>
      <Tip>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={`Copy the ${label} command`}
          onPress={() => {
            if (!navigator.clipboard) return;
            void navigator.clipboard.writeText(value).then(
              () => setCopied(true),
              () => setCopied(false),
            );
          }}
        >
          {copied ? <Check className="size-3.5" /> : <Clipboard className="size-3.5" />}
        </Button>
      </Tip>
    </div>
  );
}
