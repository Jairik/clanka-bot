import { useEffect, useRef, useState } from "react";
import { Button, Checkbox } from "@heroui/react";
import { Check, ChevronDown, Play, Square } from "lucide-react";
import { usePageData } from "../api";
import { Field, ResetButton, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { VoiceRow, VoicesView } from "../types";
import { Badge, Empty, Fold, Lede, Meta, Notice, Section, SourceBadge, cn, foldOpen, type Tone } from "../ui";
import { UploadRow } from "../upload";

const statusTone = (v: VoiceRow): Tone => (v.Stale ? "warn" : v.Unvalidated ? "bad" : "neutral");

export function VoicesPage() {
  const { data, error, set } = usePageData("voices");
  const actions = useActions<VoicesView>(set);
  const [rate, setRate] = useState("");
  const [replace, setReplace] = useState(false);
  const [playing, setPlaying] = useState("");
  const audio = useRef<HTMLAudioElement | null>(null);
  useEffect(() => setRate(data?.RateEdit ?? ""), [data?.RateEdit]);
  useEffect(() => () => audio.current?.pause(), []);

  const preview = (v: VoiceRow) => {
    audio.current?.pause();
    if (playing === v.Name) return setPlaying("");
    // A bounded sample is synthesized per request; it can take a moment.
    const a = new Audio(v.PreviewURL);
    audio.current = a;
    a.onended = a.onerror = () => setPlaying("");
    void a.play().catch(() => setPlaying(""));
    setPlaying(v.Name);
  };

  return (
    <PageFrame id="voices">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasLibrary && (
        <Section title="The voice library is not configured" defaultOpen>
          <Empty>This Dashboard was built without the voice service, so there is nothing to edit.</Empty>
        </Section>
      )}
      {data?.HasLibrary && (
        <div className="space-y-2.5">
          <Section title="Speaking voice" summary={data.ActiveName || `${data.Default} (default)`} defaultOpen flush>
            <div className="space-y-4 px-3.5 py-3">
              <Meta
                items={[
                  ["Active", <span className="font-mono">{data.ActiveName || `${data.Default} (default)`}</span>],
                  ["Default", <span className="font-mono">{data.Default}</span>],
                  ["Model revision", <span className="font-mono">{data.Revision || "not reported yet"}</span>],
                ]}
              />
              {data.HasRate && (
                <form
                  className="flex flex-wrap items-end gap-2"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void actions.run("/voices/rate", { value: rate });
                  }}
                >
                  <Field label="Speech rate (0.5–2.0, 1.0 is normal)" className="w-40">
                    <TextField label="Speech rate" value={rate} onChange={setRate} type="number" inputMode="decimal" mono />
                  </Field>
                  <Button size="sm" type="submit" isDisabled={rate === data.RateEdit} isPending={actions.pending === "/voices/rate"}>
                    Save rate
                  </Button>
                  <SourceBadge source={data.RateSource} label={data.RateLabel} />
                  {data.RateSource === "override" && <ResetButton label={`Reset to ${data.RateLower}`} onPress={() => void actions.run("/voices/rate/reset")} />}
                </form>
              )}
            </div>
          </Section>

          <Section title="Library" summary={`${data.Voices?.length ?? 0} voices`} defaultOpen flush>
            <Lede>
              Built-ins are warm in the <code>tts</code> image; custom states live on the <code>assistant-voices</code> volume. A stale state was validated against a
              different model revision and must be re-exported.
            </Lede>
            {data.Voices?.length ? (
              <ul className="divide-y divide-separator">
                {data.Voices.map((v) => (
                  <VoiceItem key={v.Name} v={v} actions={actions} canPreview={data.CanPreview} playing={playing === v.Name} onPreview={() => preview(v)} />
                ))}
              </ul>
            ) : (
              <Empty>No voices are indexed yet. Upload a .safetensors state to add one.</Empty>
            )}
            {data.CanUpload ? (
              <UploadRow
                action="/voices/upload"
                maxUpload={data.MaxUpload}
                accept=".safetensors"
                label="Upload a .safetensors voice"
                extra={(fd) => replace && fd.set("replace", "yes")}
                onView={(v) => set(v as VoicesView)}
              />
            ) : (
              <p className="border-t px-3.5 py-3 text-xs text-muted">Uploads are disabled because the speech service is not configured, so the model smoke test cannot run.</p>
            )}
            {data.CanUpload && (
              <div className="px-3.5 pb-3">
                <Checkbox isSelected={replace} onChange={setReplace}>
                  <Checkbox.Content className="text-xs text-muted">
                    <Checkbox.Control>
                      <Checkbox.Indicator />
                    </Checkbox.Control>
                    Replace an existing custom voice with the same name
                  </Checkbox.Content>
                </Checkbox>
              </div>
            )}
          </Section>
        </div>
      )}
    </PageFrame>
  );
}

function VoiceItem({ v, actions, canPreview, playing, onPreview }: { v: VoiceRow; actions: Actions<VoicesView>; canPreview: boolean; playing: boolean; onPreview: () => void }) {
  const [note, setNote] = useState(v.Note);
  useEffect(() => setNote(v.Note), [v.Note]);
  return (
    <li id={v.Anchor}>
      <Fold
        triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2.5 hover:bg-surface-secondary"
        bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
        trigger={
          <>
            <span className={cn("flex size-6 shrink-0 items-center justify-center border", v.Active && "border-foreground bg-foreground text-background")}>
              {v.Active && <Check className="size-3.5" />}
            </span>
            <div className="min-w-0 flex-1">
              <div className="truncate font-mono text-sm">{v.Name}</div>
              <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[11px] text-muted">
                <Badge tone={statusTone(v)}>{v.Status}</Badge>
                {v.Origin}
              </div>
            </div>
            {v.Active && <span className="hidden text-xs sm:inline">Active</span>}
            <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
          </>
        }
      >
        <div className="space-y-3">
          <Meta items={[["Revision", <span className="font-mono">{v.Revision}</span>], ["Validated", v.Validated], ["Added", v.Added]]} />
          {v.StaleNotice && <Notice error={v.StaleNotice} />}
          {v.LastError && <Notice error={`Last validation error: ${v.LastError}`} />}
          <div className="flex flex-wrap items-center gap-2">
            {v.Active ? (
              <Badge>Active voice</Badge>
            ) : (
              <Button size="sm" isDisabled={!v.Ready} isPending={actions.pending === v.SelectAction} onPress={() => void actions.run(v.SelectAction)}>
                Use this voice
              </Button>
            )}
            <Button size="sm" variant="secondary" isDisabled={!v.Ready || !canPreview} onPress={onPreview}>
              {playing ? <Square className="size-3.5 fill-current" /> : <Play className="size-3.5" />}
              {playing ? "Stop" : "Preview"}
            </Button>
            {!v.Ready && <span className="text-xs text-muted">Disabled until this state is validated.</span>}
            {v.Ready && !canPreview && <span className="text-xs text-muted">Preview needs the speech service.</span>}
          </div>
          {!v.Builtin && (
            <form
              className="space-y-2"
              onSubmit={(e) => {
                e.preventDefault();
                void actions.run(v.NoteAction, { note });
              }}
            >
              <Field label="Source / consent note (optional; recorded, not enforced)">
                <TextField label="Note" value={note} onChange={setNote} multiline rows={2} />
              </Field>
              <Button size="sm" variant="secondary" type="submit" isDisabled={note === v.Note} isPending={actions.pending === v.NoteAction}>
                Save note
              </Button>
            </form>
          )}
        </div>
      </Fold>
    </li>
  );
}
