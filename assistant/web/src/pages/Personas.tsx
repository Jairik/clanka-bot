import { useEffect, useState } from "react";
import { Button } from "@heroui/react";
import { Check, ChevronDown, Plus } from "lucide-react";
import { usePageData } from "../api";
import { Field, SelectField, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { PersonaForm, PersonasView } from "../types";
import { Badge, Empty, Fold, Lede, Notice, Section, cn, foldOpen } from "../ui";

export function PersonasPage() {
  const { data, error, set } = usePageData("personas");
  const actions = useActions<PersonasView>(set);
  return (
    <PageFrame id="personas">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasService && (
        <Section title="The persona library is not configured" defaultOpen>
          <Empty>This Dashboard was built without the persona service, so there is nothing to edit.</Empty>
        </Section>
      )}
      {data?.HasService && (
        <div className="space-y-2.5">
          <Section title="Active persona" summary={data.Active?.Name ?? "none"} tone={data.Active && !data.Active.Exact ? "warn" : undefined} defaultOpen flush>
            {data.Active ? (
              <div className="space-y-3 px-3.5 py-3">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-serif text-xl">{data.Active.Name}</span>
                  {data.Active.Exact ? <Badge>Exact bundle</Badge> : <Badge tone="warn">Overridden or edited</Badge>}
                </div>
                <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
                  <dt className="text-muted">Persona voice</dt>
                  <dd className="font-mono">{data.Active.Voice}</dd>
                  <dt className="text-muted">Speaking voice</dt>
                  <dd className="font-mono">{data.Active.EffectiveVoice || "unavailable, using the default"}</dd>
                </dl>
                {(data.Active.Notes ?? []).map((n) => (
                  <p key={n} className="border-l-2 border-warning pl-2.5 text-xs text-muted">
                    {n}
                  </p>
                ))}
                <div className="flex flex-wrap items-center gap-2">
                  <Button size="sm" variant="secondary" isPending={actions.pending === "/personas/active/reset"} onPress={() => void actions.run("/personas/active/reset")}>
                    Clear the active persona
                  </Button>
                  <span className="text-[11px] text-subtle">Clearing removes only the marker; the applied voice and wording stay.</span>
                </div>
              </div>
            ) : (
              <Empty>No persona is selected. The assistant keeps its current voice and wording.</Empty>
            )}
          </Section>

          <Section title="Library" summary={`${data.Rows?.length ?? 0} personas`} defaultOpen flush>
            <Lede>Selecting copies the voice and wording into the live settings in one step. Edits apply on the next turn; select again to re-apply the voice and wording.</Lede>
            {data.Rows?.length ? (
              <ul className="divide-y divide-separator">
                {data.Rows.map((r) => (
                  <li key={r.Name} id={r.Edit.FormID}>
                    <Fold
                      triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2.5 hover:bg-surface-secondary"
                      bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
                      trigger={
                        <>
                          <span className={cn("flex size-6 shrink-0 items-center justify-center border", r.Active && "border-foreground bg-foreground text-background")}>
                            {r.Active && <Check className="size-3.5" />}
                          </span>
                          <div className="min-w-0 flex-1">
                            <div className="truncate text-sm">{r.Name}</div>
                            <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[11px] text-muted">
                              <span className="font-mono">{r.Voice}</span>
                              {r.VoiceStatus !== "ready" && r.VoiceStatus !== "builtin" && <Badge tone="warn">voice {r.VoiceStatus}</Badge>}
                            </div>
                          </div>
                          {!r.Active && (
                            <span
                              role="button"
                              tabIndex={0}
                              className="border px-2.5 py-1 text-xs hover:bg-default"
                              onClick={(e) => {
                                e.stopPropagation();
                                void actions.run(r.SelectAction);
                              }}
                              onKeyDown={(e) => {
                                if (e.key === "Enter" || e.key === " ") {
                                  e.preventDefault();
                                  e.stopPropagation();
                                  void actions.run(r.SelectAction);
                                }
                              }}
                            >
                              {actions.pending === r.SelectAction ? "Selecting…" : "Select"}
                            </span>
                          )}
                          <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
                        </>
                      }
                    >
                      <PersonaEditor form={r.Edit} actions={actions} />
                    </Fold>
                  </li>
                ))}
              </ul>
            ) : (
              <Empty>No personas are stored yet.</Empty>
            )}
          </Section>

          <Section title={data.New.Heading || "Create a persona"} right={<Plus className="size-4 text-muted" />} flush>
            <Lede>Only ready voices can be saved. Creating a persona does not select it.</Lede>
            <div className="px-3.5 py-3">
              <PersonaEditor form={data.New} actions={actions} />
            </div>
          </Section>
        </div>
      )}
    </PageFrame>
  );
}

/** The one persona form, for create and edit. A refused save comes back with the input preserved. */
function PersonaEditor({ form, actions }: { form: PersonaForm; actions: Actions<PersonasView> }) {
  const init = () => ({
    name: form.Name,
    voice: form.Voice || (form.Voices ?? []).find((v) => v.Selected)?.Name || form.Voices?.[0]?.Name || "",
    personality: form.Personality,
    filler: form.Filler,
    unavailable: form.Unavailable,
  });
  const [f, setF] = useState(init);
  // Re-seed when the server's copy changes (after a save or a refused write).
  const key = JSON.stringify([form.Name, form.Voice, form.Personality, form.Filler, form.Unavailable]);
  useEffect(() => setF(init()), [key]); // eslint-disable-line react-hooks/exhaustive-deps
  const up = (k: keyof typeof f) => (v: string) => setF((s) => ({ ...s, [k]: v }));
  const voices = form.Voices ?? [];
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        const body: Record<string, string> = { voice: f.voice, personality: f.personality, filler: f.filler, unavailable: f.unavailable };
        if (!form.NameLocked) body.name = f.name;
        void actions.run(form.Action, body);
      }}
    >
      {!form.NameLocked && (
        <Field label="Persona name">
          <TextField label="Persona name" value={f.name} onChange={up("name")} />
        </Field>
      )}
      <Field label="Voice">
        {voices.length ? (
          <SelectField label="Voice" value={f.voice} onChange={up("voice")} options={voices.map((v) => ({ value: v.Name, label: v.Label }))} className="sm:w-72" />
        ) : (
          <p className="text-xs text-danger">No ready voices. Add one on the Voices page first.</p>
        )}
      </Field>
      <Field label="Personality prompt">
        <TextField label="Personality prompt" value={f.personality} onChange={up("personality")} multiline rows={5} />
      </Field>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Filler phrase">
          <TextField label="Filler phrase" value={f.filler} onChange={up("filler")} multiline rows={2} />
        </Field>
        <Field label="Unavailable message">
          <TextField label="Unavailable message" value={f.unavailable} onChange={up("unavailable")} multiline rows={2} />
        </Field>
      </div>
      <Button size="sm" type="submit" isDisabled={!voices.length} isPending={actions.pending === form.Action}>
        {form.Submit || "Save"}
      </Button>
    </form>
  );
}
