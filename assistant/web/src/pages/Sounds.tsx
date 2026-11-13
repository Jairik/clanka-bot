import { useRef, useState } from "react";
import { Button } from "@heroui/react";
import { Play, Speaker, Upload } from "lucide-react";
import { usePageData } from "../api";
import { ResetButton, SelectField, SettingRow, useActions } from "../forms";
import { PageFrame } from "../shell";
import type { SoundsView } from "../types";
import { Badge, Empty, Lede, Notice, Section, SourceBadge } from "../ui";

export function SoundsPage() {
  const { data, error, set } = usePageData("sounds");
  const actions = useActions<SoundsView>(set);
  const [playing, setPlaying] = useState("");
  const audio = useRef<HTMLAudioElement | null>(null);

  const preview = (name: string, url: string) => {
    audio.current?.pause();
    if (playing === name) return setPlaying("");
    const a = new Audio(url);
    audio.current = a;
    a.onended = () => setPlaying("");
    void a.play();
    setPlaying(name);
  };

  return (
    <PageFrame id="sounds">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasLibrary && (
        <Section title="The sound library is not configured" defaultOpen>
          <Empty>This Dashboard was built without the sound service, so there is nothing to edit.</Empty>
        </Section>
      )}
      {data?.HasLibrary && (
        <div className="space-y-2.5">
          <Section title="Library" summary={`${data.Sounds?.length ?? 0} sounds`} defaultOpen flush>
            <Lede>
              Play previews the stored audio here. On Pi queues it on the speaker after the current reply and any pending Confirmation; a new turn cancels it.
            </Lede>
            {data.Sounds?.length ? (
              <ul className="divide-y divide-separator">
                {data.Sounds.map((s) => {
                  const pi = `/sounds/preview/${encodeURIComponent(s.Name)}`;
                  return (
                    <li key={s.Name} className="flex min-h-14 items-center gap-3 px-3.5 py-2">
                      <Button isIconOnly size="sm" variant={playing === s.Name ? "primary" : "secondary"} aria-label={`Preview ${s.Name}`} onPress={() => preview(s.Name, s.AudioURL)}>
                        <Play className="size-3.5" />
                      </Button>
                      <div className="min-w-0 flex-1">
                        <div className="truncate font-mono text-sm">{s.Name}</div>
                        <div className="truncate text-[11px] text-muted">
                          {[s.Duration, s.Origin, s.Added && `added ${s.Added}`].filter(Boolean).join(" · ")}
                        </div>
                      </div>
                      <Button size="sm" variant="ghost" isPending={actions.pending === pi} onPress={() => void actions.run(pi)} aria-label={`Play ${s.Name} on the Pi`}>
                        <Speaker className="size-3.5" /> <span className="hidden sm:inline">On Pi</span>
                      </Button>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <Empty>No sounds are indexed yet.</Empty>
            )}
            <UploadRow maxUpload={data.MaxUpload} pending={actions.pending === "/sounds/upload"} onUpload={(f) => actions.run("/sounds/upload", f)} />
          </Section>

          <Section title="Cue roles" summary={`${data.Roles?.length ?? 0} roles`} flush>
            <Lede>Each role is one live setting. A sound longer than the role's cap is rejected.</Lede>
            {data.HasRoles ? (
              <ul className="divide-y divide-separator">
                {(data.Roles ?? []).map((r) => {
                  const save = `/sounds/roles/${encodeURIComponent(r.Role)}`;
                  return (
                    <li key={r.Role} className="flex flex-col gap-2 px-3.5 py-3 sm:flex-row sm:items-center">
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <span className="text-sm">{r.Label}</span>
                          <SourceBadge source={r.Source} label={r.SourceLabel} />
                          <Badge>max {r.Cap}</Badge>
                        </div>
                        <div className="font-mono text-[11px] text-subtle">{r.Key}</div>
                      </div>
                      <div className="flex items-center gap-2">
                        <SelectField
                          label={`${r.Label} sound`}
                          value={(r.Options ?? []).find((o) => o.Selected)?.Value ?? ""}
                          options={(r.Options ?? []).map((o) => ({ value: o.Value, label: o.Label }))}
                          isDisabled={actions.pending === save}
                          onChange={(v) => void actions.run(save, { value: v })}
                        />
                        {r.Override && <ResetButton label="Reset" onPress={() => void actions.run(`${save}/reset`)} />}
                      </div>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <Empty>No Cue role settings are defined.</Empty>
            )}
          </Section>

          {data.HasRing ? (
            (data.RingKinds ?? []).map((k) => (
              <Section key={k.Kind} title={`${k.Label} Ring`} summary={`${k.Settings?.length ?? 0} settings`} flush>
                <ul className="divide-y divide-separator">
                  {(k.Settings ?? []).map((s) => (
                    <li key={s.Key}>
                      <SettingRow s={s} actions={actions} />
                    </li>
                  ))}
                </ul>
              </Section>
            ))
          ) : (
            <Section title="Ring settings">
              <Empty>No per-kind Ring settings are defined yet.</Empty>
            </Section>
          )}
        </div>
      )}
    </PageFrame>
  );
}

/** File picker + upload. `accept` mirrors what the server decodes. */
export function UploadRow({
  maxUpload, pending, onUpload, accept = ".wav,.mp3,audio/wav,audio/mpeg", label = "Upload a WAV or MP3", extra,
}: {
  maxUpload: string;
  pending: boolean;
  onUpload: (f: FormData) => Promise<boolean>;
  accept?: string;
  label?: string;
  extra?: (fd: FormData) => void;
}) {
  const input = useRef<HTMLInputElement>(null);
  return (
    <div className="flex flex-wrap items-center gap-3 border-t px-3.5 py-3">
      <input
        ref={input}
        type="file"
        accept={accept}
        className="hidden"
        onChange={async (e) => {
          const file = e.target.files?.[0];
          if (!file) return;
          const fd = new FormData();
          fd.set("file", file);
          extra?.(fd);
          await onUpload(fd);
          e.target.value = "";
        }}
      />
      <Button size="sm" variant="secondary" isPending={pending} onPress={() => input.current?.click()}>
        <Upload className="size-3.5" /> {label}
      </Button>
      <span className="text-[11px] text-subtle">Up to {maxUpload}.</span>
    </div>
  );
}
