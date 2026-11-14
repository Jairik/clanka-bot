import { useEffect, useState } from "react";
import { Button } from "@heroui/react";
import { getJSON, postForm } from "../api";
// Settings shares the global equalizer with every Pi audio client.
import { Equalizer } from "./Equalizer";
import { Lock } from "lucide-react";
import { usePageData } from "../api";
import { ResetButton, SelectField, SettingRow, Toggle, useActions } from "../forms";
import { PageFrame } from "../shell";
import type { Setting, SettingTool, SettingsView } from "../types";
import { Badge, Empty, Lede, Notice, Section, SourceBadge } from "../ui";
import { useWordingPlayer, WordingPreview } from "../wording-preview";

export function SettingsPage() {
  const { data, error, set } = usePageData("settings");
  const actions = useActions<SettingsView>(set);
  // One audio element for every wording preview on the page.
  const wording = useWordingPlayer();
  const list = (items: Setting[] | null) => (
    <ul className="divide-y divide-separator">
      {(items ?? []).map((s) => (
        <li key={s.Key} id={s.Anchor}>
          <SettingRow s={s} actions={actions} />
        </li>
      ))}
    </ul>
  );

  return (
    <PageFrame id="settings">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasSettings && (
        <Section title="Settings are not configured" defaultOpen>
          <Empty>This Dashboard was built without the settings service, so there is nothing to edit.</Empty>
        </Section>
      )}
      {data?.HasSettings && (
        <div className="space-y-2.5">
          <Equalizer />
          <Section title="Tools" summary={`${(data.Tools ?? []).filter((t) => t.Enabled).length}/${data.Tools?.length ?? 0} enabled`} defaultOpen flush>
            <Lede>
              Enable or disable a Tool, or tighten its Permission. A Permission can only move towards Confirm; Local-only is declared in code. Changes apply on the
              next request.
            </Lede>
            {data.Tools?.length ? (
              <ul className="divide-y divide-separator">
                {data.Tools.map((t) => (
                  <ToolRow key={t.Name} t={t} actions={actions} />
                ))}
              </ul>
            ) : (
              <Empty>No Tools are registered.</Empty>
            )}
          </Section>

          <Section
            title="Wake word"
            summary={data.Wakeword.Enabled ? `${data.Wakeword.Models?.length ?? 0} models` : "off"}
            flush
          >
            <Lede>{data.Wakeword.Note}</Lede>
            {data.Wakeword.Models?.length ? (
              <ul className="divide-y divide-separator">
                {data.Wakeword.Models.map((model) => (
                  <li key={model.Name} className="flex items-center justify-between gap-3 px-3.5 py-3">
                    <span className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-sm">{model.Name}</span>
                      {model.Command && <Badge tone={model.Enabled ? "neutral" : "warn"}>command{model.Enabled ? "" : " · off"}</Badge>}
                    </span>
                    <span className="text-xs text-muted">threshold {model.Threshold}</span>
                  </li>
                ))}
              </ul>
            ) : (
              <Empty>No wake model is configured.</Empty>
            )}
          </Section>

          <Section title="Agent wording" summary={`${data.Agent?.length ?? 0} settings`} flush>
            <Lede>Editable as ordinary text. A blank line in a list is ignored; an empty list is valid where the setting allows it.</Lede>
            {data.Agent?.length ? (
              // Only the Agent wording rows have a live preview, so they get
              // their own mapping; `list` stays shared by every other section.
              <ul className="divide-y divide-separator">
                {data.Agent.map((s) => (
                  <li key={s.Key} id={s.Anchor}>
                    <SettingRow
                      s={s}
                      actions={actions}
                      preview={(draft) => (
                        <WordingPreview
                          s={s}
                          draft={draft}
                          voice={data.Voice}
                          canPreview={data.CanPreview === true}
                          maxRunes={data.PreviewMaxRunes}
                          player={wording}
                        />
                      )}
                    />
                  </li>
                ))}
              </ul>
            ) : (
              <Empty>No Agent wording settings are defined.</Empty>
            )}
          </Section>

          <Section title="Per-Tool wording" summary={`${data.ToolWording?.length ?? 0} tools`} flush>
            {data.ToolWording?.length ? (
              <div className="divide-y divide-separator">
                {data.ToolWording.map((w) => (
                  <div key={w.Name} id={w.Anchor}>
                    <div className="bg-surface-secondary px-3.5 py-1.5 font-mono text-xs text-muted">{w.Name}</div>
                    {list([w.Control, w.Examples].filter(Boolean) as Setting[])}
                  </div>
                ))}
              </div>
            ) : (
              <Empty>No per-Tool wording settings are defined.</Empty>
            )}
          </Section>

          {!!data.Quiet?.length && (
            <Section title="Quiet hours" summary={data.Quiet.find((s) => s.FormKind === "bool")?.Display} flush>
              <Lede>
                One daily window in the effective timezone. Inside it Announcements are held as one summary, Notifications are muted, and replies, Timers and
                Reminders play at the quiet volume. Alarms still Ring normally.
              </Lede>
              {list(data.Quiet)}
            </Section>
          )}

          {!!data.Live?.length && (
            <Section title="Conversation and other live settings" summary={`${data.Live.length} settings`} flush>
              <Lede>Every other setting the service exposes is listed here automatically.</Lede>
              <Lede>Chained commands run in order. Follow-up seconds: 0 disables listening without a wake word; music always requires a wake word.</Lede>
              {list(data.Live)}
              <QueuedActions />
            </Section>
          )}

          {!!data.Restart?.length && (
            <Section title="Restart required" summary={`${data.Restart.length} settings`} tone="warn" flush>
              <Lede>These values are stored now, but the assistant only reads them at startup. Restart it for a change to take effect.</Lede>
              {list(data.Restart)}
            </Section>
          )}
        </div>
      )}
    </PageFrame>
  );
}

function ToolRow({ t, actions }: { t: SettingTool; actions: ReturnType<typeof useActions<SettingsView>> }) {
  const save = `/settings/tools/${encodeURIComponent(t.Name)}`;
  const reset = `${save}/reset`;
  const busy = actions.pending === save || actions.pending === reset;
  return (
    <li className="flex flex-col gap-3 px-3.5 py-3 sm:flex-row sm:items-center">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className={t.Enabled ? "font-mono text-sm" : "font-mono text-sm text-muted line-through"}>{t.Name}</span>
          {t.LocalOnly && (
            <Badge>
              <Lock className="size-3" /> Local only
            </Badge>
          )}
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-1.5 text-[11px] text-muted">
          Code: {t.DeclaredLabel}
          <span>·</span> enabled <SourceBadge source={t.EnabledSource} label={t.EnabledLabel} />
          <span>·</span> permission <SourceBadge source={t.PermissionSource} label={t.PermissionLabelSource} />
        </div>
      </div>
      <div className="flex items-center gap-2">
        <SelectField
          label={`${t.Name} Permission`}
          className="min-w-0 flex-1 sm:w-40 sm:flex-none"
          value={t.Permission}
          isDisabled={busy}
          options={(t.Options ?? []).map((o) => ({ value: o.Value, label: o.Label }))}
          onChange={(v) => v !== t.Permission && void actions.run(save, { field: "permission", value: v })}
        />
        {t.PermissionOverride && <ResetButton label="Permission" onPress={() => void actions.run(reset, { field: "permission" })} />}
        <Toggle label={`${t.Name} enabled`} isSelected={t.Enabled} isDisabled={busy} onChange={(v) => void actions.run(save, { field: "enabled", value: String(v) })} />
        {t.EnabledOverride && <ResetButton label="Enabled" onPress={() => void actions.run(reset, { field: "enabled" })} />}
      </div>
    </li>
  );
}

// Pending orchestration shares the authenticated API and stays cancellable from Settings.
function QueuedActions() {
  const [items, setItems] = useState<{ id: number; event: string; text: string; expires: string }[]>([]);
  const [error, setError] = useState("");
  const refresh = () => getJSON<typeof items>("/api/deferred").then(setItems).catch((e) => setError(String(e)));
  useEffect(() => {
    void refresh();
    const timer = setInterval(refresh, 3000);
    return () => clearInterval(timer);
  }, []);
  return (
    <div className="p-3.5 space-y-2">
      <p>Queued actions · {items.length}/8</p>
      {error && <p role="alert">{error}</p>}
      {items.map((item) => (
        <div key={item.id} className="flex items-center justify-between gap-3">
          <span>{item.text} · after {item.event} · expires {new Date(item.expires).toLocaleTimeString()}</span>
          <Button variant="secondary" onPress={async () => {
            try {
              const result = await postForm("/api/deferred", { id: String(item.id) });
              if (!result.ok) throw new Error(result.text);
              await refresh();
            } catch (e) { setError(String(e)); }
          }}>Cancel</Button>
        </div>
      ))}
    </div>
  );
}
