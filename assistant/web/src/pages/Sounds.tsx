import { useEffect, useRef, useState, type FormEvent } from "react";
import { Button } from "@heroui/react";
import { Play, Speaker, Trash2 } from "lucide-react";
import { usePageData } from "../api";
import { Field, ResetButton, SelectField, SettingRow, TextField, useActions } from "../forms";
import { PageFrame } from "../shell";
import type { SoundsView } from "../types";
import { UploadRow } from "../upload";
import { Empty, Lede, Notice, Section, SourceBadge, Tip } from "../ui";

export function SoundsPage() {
  const { data, error, set } = usePageData("sounds");
  const actions = useActions<SoundsView>(set);
  const [playing, setPlaying] = useState("");
  const [confirming, setConfirming] = useState("");
  const [youtube, setYoutube] = useState("");
  const audio = useRef<HTMLAudioElement | null>(null);
  useEffect(() => () => audio.current?.pause(), []);

  // installYouTube posts one public video link. Success refreshes the library
  // the same way an upload does and clears the field; a refusal keeps the link
  // so it can be edited, and the page notice shows the server's error text.
  const installYouTube = (event: FormEvent) => {
    event.preventDefault();
    const url = youtube.trim();
    if (!url) return;
    void actions.run("/sounds/youtube", { url }).then((ok) => {
      if (ok) setYoutube("");
    });
  };

  const preview = (name: string, url: string) => {
    audio.current?.pause();
    if (playing === name) return setPlaying("");
    const a = new Audio(url);
    audio.current = a;
    // A blocked autoplay or a missing file must not leave the button lit.
    a.onended = a.onerror = () => setPlaying("");
    void a.play().catch(() => setPlaying(""));
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
                      <Tip>
                        <Button isIconOnly size="sm" variant={playing === s.Name ? "primary" : "secondary"} aria-label={`Preview ${s.Name}`} onPress={() => preview(s.Name, s.AudioURL)}>
                          <Play className="size-3.5" />
                        </Button>
                      </Tip>
                      <div className="min-w-0 flex-1">
                        <div className="truncate font-mono text-sm">{s.Name}</div>
                        <div className="truncate text-[11px] text-muted">
                          {[s.Duration, s.Origin, s.Added && `added ${s.Added}`].filter(Boolean).join(" · ")}
                        </div>
                      </div>
                      {confirming === s.Name ? (
                        <>
                          <span className="text-xs text-danger">Delete?</span>
                          <Button
                            size="sm"
                            variant="danger"
                            isPending={actions.pending === s.DeleteAction}
                            onPress={() => (setConfirming(""), void actions.run(s.DeleteAction))}
                            aria-label={`Confirm delete ${s.Name}`}
                          >
                            Delete
                          </Button>
                          <Button size="sm" variant="ghost" onPress={() => setConfirming("")}>
                            Cancel
                          </Button>
                        </>
                      ) : (
                        <>
                          <Tip>
                            <Button size="sm" variant="ghost" isPending={actions.pending === pi} onPress={() => void actions.run(pi)} aria-label={`Play ${s.Name}: On Pi`}>
                              <Speaker className="size-3.5" /> <span className="hidden sm:inline">On Pi</span>
                            </Button>
                          </Tip>
                          {s.DeleteAction && (
                            <Tip>
                              <Button
                                isIconOnly
                                size="sm"
                                variant="danger-soft"
                                isPending={actions.pending === s.DeleteAction}
                                onPress={() => setConfirming(s.Name)}
                                aria-label={`Delete ${s.Name}`}
                              >
                                <Trash2 className="size-3.5" />
                              </Button>
                            </Tip>
                          )}
                        </>
                      )}
                    </li>
                  );
                })}
              </ul>
            ) : (
              <Empty>No sounds are indexed yet.</Empty>
            )}
            <UploadRow
              action="/sounds/upload"
              maxUpload={data.MaxUpload}
              accept=".wav,.mp3,audio/wav,audio/mpeg"
              label="Upload a WAV or MP3"
              onView={(v) => set(v as SoundsView)}
            />
            <form className="flex flex-col gap-2 border-t px-3.5 py-3 sm:flex-row sm:items-end" onSubmit={installYouTube}>
              <Field label="YouTube URL" hint={`Downloads the audio once into this library, up to ${data.MaxUpload}.`} className="min-w-0 w-full sm:flex-1">
                <TextField label="YouTube URL" value={youtube} onChange={setYoutube} placeholder="https://www.youtube.com/watch?v=…" autoComplete="off" />
              </Field>
              <Button
                type="submit"
                size="sm"
                variant="secondary"
                className="w-full sm:w-auto"
                isDisabled={!youtube.trim() || actions.pending === "/sounds/youtube"}
                isPending={actions.pending === "/sounds/youtube"}
              >
                Install from YouTube
              </Button>
            </form>
          </Section>

          <Section title="Cue roles" summary={`${data.Roles?.length ?? 0} roles`} flush>
            <Lede>Each role is one live setting. Sounds of any length are accepted.</Lede>
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
