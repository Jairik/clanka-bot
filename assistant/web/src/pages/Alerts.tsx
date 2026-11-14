import { useEffect, useState } from "react";
import { Button, Checkbox } from "@heroui/react";
import { AlarmClock, BellRing, ChevronDown, Hourglass, StickyNote, X } from "lucide-react";
import { usePageData } from "../api";
import { Field, SelectField, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { AlarmEditor, AlertsView } from "../types";
import { Badge, Empty, Fold, Lede, Meta, Notice, Section, cn, foldOpen, Tip } from "../ui";

const KIND_ICON: Record<string, typeof AlarmClock> = { timer: Hourglass, alarm: AlarmClock, reminder: StickyNote };

export function AlertsPage() {
  // Timers count down and Rings start on their own; keep the list current.
  const { data, error, set } = usePageData("alerts", { poll: 15_000 });
  const actions = useActions<AlertsView>(set);
  return (
    <PageFrame id="alerts">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasManagers && (
        <Section title="Scheduled alerts are not configured" defaultOpen>
          <Empty>This Dashboard was built without the Timer, Alarm and Reminder modules, so there is nothing to show.</Empty>
        </Section>
      )}
      {data?.HasManagers && (
        <div className="space-y-2.5">
          <Section title="Active" summary={`${data.Alerts?.length ?? 0} scheduled · ${data.ZoneName}`} defaultOpen flush>
            {data.Alerts?.length ? (
              <ul className="divide-y divide-separator">
                {data.Alerts.map((a) => {
                  const I = KIND_ICON[a.KindKey] ?? BellRing;
                  return (
                    <li key={a.CancelAction} className="flex min-h-14 items-center gap-3 px-3.5 py-2.5">
                      <I className="size-4 shrink-0 text-muted" strokeWidth={1.5} />
                      <div className="min-w-0 flex-1">
                        <div className="truncate text-sm">{a.Label || a.Kind}</div>
                        <div className="truncate text-[11px] text-muted">
                          {a.Kind}
                          {a.Detail && ` · ${a.Detail}`}
                        </div>
                      </div>
                      <time dateTime={a.NextISO} className="shrink-0 text-right text-xs tabular-nums">
                        {a.Next}
                      </time>
                      <Tip>
                        <Button
                          isIconOnly
                          size="sm"
                          variant="danger-soft"
                          aria-label={a.CancelLabel || `Cancel ${a.Label}`}
                          isPending={actions.pending === a.CancelAction}
                          onPress={() => void actions.run(a.CancelAction)}
                        >
                          <X className="size-3.5" />
                        </Button>
                      </Tip>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <Empty>No Timers, Alarms or Reminders are set.</Empty>
            )}
          </Section>

          <Section title="Alarm overrides" summary={`${data.Alarms?.length ?? 0} alarms`} flush>
            <Lede>
              A per-Alarm sound, volume or ramp replaces the kind default for that Alarm only. Blank keeps the current value; an explicit 0 is real (mute, or no ramp).
            </Lede>
            {data.Alarms?.length ? (
              <ul className="divide-y divide-separator">
                {data.Alarms.map((a) => (
                  <AlarmItem key={a.ID} a={a} actions={actions} />
                ))}
              </ul>
            ) : (
              <Empty>No Alarms are set.</Empty>
            )}
          </Section>

          <Section title="Missed" summary={data.Missed?.length ? `${data.Missed.length} recent` : "none"} tone={data.Missed?.length ? "warn" : undefined} flush>
            <Lede>The most recent {data.MissedLimit} Rings nobody stopped, newest first, in {data.ZoneName}.</Lede>
            {data.Missed?.length ? (
              <ul className="divide-y divide-separator">
                {data.Missed.map((m, i) => (
                  <li key={i} className="px-3.5 py-2.5">
                    <div className="flex items-center gap-2">
                      <span className="min-w-0 flex-1 truncate text-sm">{m.Label || m.Kind}</span>
                      <Badge>{m.Kind}</Badge>
                    </div>
                    <Meta
                      className="mt-1"
                      items={[
                        ["Scheduled", <time dateTime={m.ScheduledISO}>{m.Scheduled}</time>],
                        ["Fired", <time dateTime={m.FiredISO}>{m.Fired}</time>],
                        ["Ended", m.EndedISO ? <time dateTime={m.EndedISO}>{m.Ended}</time> : m.Ended],
                        ["Reason", m.EndedBy],
                      ]}
                    />
                  </li>
                ))}
              </ul>
            ) : (
              <Empty>No missed alerts in the recent history.</Empty>
            )}
          </Section>
        </div>
      )}
    </PageFrame>
  );
}

function AlarmItem({ a, actions }: { a: AlarmEditor; actions: Actions<AlertsView> }) {
  const seed = () => ({
    sound: (a.Sounds ?? []).find((s) => s.Selected)?.Value ?? "",
    volume: a.Edit.Volume,
    ramp: a.Edit.Ramp,
    volumeReset: a.Edit.VolumeReset,
    rampReset: a.Edit.RampReset,
  });
  const [f, setF] = useState(seed);
  const key = JSON.stringify([a.Edit, a.SoundCurrent, a.VolumeCurrent, a.RampCurrent]);
  useEffect(() => setF(seed()), [key]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <li id={`alarm-overrides-${a.ID}`}>
      <Fold
        triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2.5 hover:bg-surface-secondary"
        bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
        trigger={
          <>
            <AlarmClock className="size-4 shrink-0 text-muted" strokeWidth={1.5} />
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm">{a.Title}</div>
              <div className="truncate text-[11px] text-muted">
                {a.Schedule} · next <time dateTime={a.NextISO}>{a.Next}</time>
              </div>
            </div>
            {a.HasOverride && <Badge className="bg-foreground text-background">Overrides</Badge>}
            <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
          </>
        }
      >
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            const body: Record<string, string> = { volume: f.volume, ramp: f.ramp };
            if (a.HasLibrary) body.sound = f.sound;
            if (f.volumeReset) body.volume_reset = "1";
            if (f.rampReset) body.ramp_reset = "1";
            void actions.run(a.Action, body);
          }}
        >
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Sound" hint={<>Now {a.SoundEffective} · default {a.SoundDefault} · cap {a.SoundCap}</>}>
              {a.HasLibrary ? (
                <SelectField label="Sound" value={f.sound} onChange={(v) => setF((s) => ({ ...s, sound: v }))} options={(a.Sounds ?? []).map((o) => ({ value: o.Value, label: o.Label }))} className="sm:w-full" />
              ) : (
                <p className="text-xs text-muted">The sound library is unavailable.</p>
              )}
            </Field>
            <Field label="Volume (0–100)" hint={<>Now {a.VolumeEffective} · default {a.VolumeDefault}</>}>
              <TextField label="Volume" value={f.volume} placeholder={a.VolumeCurrent} inputMode="numeric" mono onChange={(v) => setF((s) => ({ ...s, volume: v }))} />
              <ResetBox label="Use the default volume" checked={f.volumeReset} onChange={(v) => setF((s) => ({ ...s, volumeReset: v }))} />
            </Field>
            <Field label="Ramp (seconds)" hint={<>Now {a.RampEffective} · default {a.RampDefault}</>}>
              <TextField label="Ramp" value={f.ramp} placeholder={a.RampCurrent} inputMode="numeric" mono onChange={(v) => setF((s) => ({ ...s, ramp: v }))} />
              <ResetBox label="Use the default ramp" checked={f.rampReset} onChange={(v) => setF((s) => ({ ...s, rampReset: v }))} />
            </Field>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" type="submit" isPending={actions.pending === a.Action}>
              Save overrides
            </Button>
            {a.HasOverride && (
              <Button size="sm" variant="ghost" isPending={actions.pending === a.ResetAction} onPress={() => void actions.run(a.ResetAction)}>
                Clear all overrides
              </Button>
            )}
          </div>
        </form>
      </Fold>
    </li>
  );
}

function ResetBox({ label, checked, onChange }: { label: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <Checkbox isSelected={checked} onChange={onChange} className="mt-1.5">
      <Checkbox.Content className="text-[11px] text-muted">
        <Checkbox.Control>
          <Checkbox.Indicator />
        </Checkbox.Control>
        {label}
      </Checkbox.Content>
    </Checkbox>
  );
}
