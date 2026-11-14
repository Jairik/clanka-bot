// The Phrase book: the short confirmation each Tool speaks, per Persona.
// Selecting a Persona loads its own overrides; the shipped defaults are what
// is spoken when no Persona is active.
import { useEffect, useState } from "react";
import { Button } from "@heroui/react";
import { getJSON, usePageData } from "../api";
import { ResetButton, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { PhrasebookSlot, PhrasebookView } from "../types";
import { Empty, Lede, Notice, Section, SourceBadge, cn } from "../ui";

export function PhrasebookPage() {
  const { data, error, set } = usePageData("phrasebook");
  const actions = useActions<PhrasebookView>(set);
  const [selectError, setSelectError] = useState("");
  const [switching, setSwitching] = useState("");

  // One dismissal clears both the write result and a persona-switch failure.
  const dismiss = () => {
    actions.clear();
    setSelectError("");
  };

  // Switching Persona is a plain read that answers the whole view. `query`
  // only applies to the first load, so it cannot be used here.
  const pick = async (name: string) => {
    if (!data || switching || name === data.Selected) return;
    // A flash from the previous Persona ("Phrase saved.") must not sit over the
    // newly selected one.
    actions.clear();
    setSelectError("");
    setSwitching(name);
    try {
      const view = await getJSON<PhrasebookView>("/api/ui/phrasebook?persona=" + encodeURIComponent(name));
      set(view);
    } catch (e) {
      setSelectError(e instanceof Error ? e.message : String(e));
    } finally {
      setSwitching("");
    }
  };

  return (
    <PageFrame id="phrasebook">
      <Notice error={error || actions.error || selectError} flash={actions.flash} onDismiss={dismiss} />
      {data && !data.HasService && (
        <Section title="The phrase book is not configured" defaultOpen>
          <Empty>This Dashboard was built without the phrase book or persona service, so there is nothing to edit.</Empty>
        </Section>
      )}
      {data?.HasService && <PhrasebookBody data={data} actions={actions} switching={switching} onPick={pick} />}
    </PageFrame>
  );
}

function PhrasebookBody({
  data,
  actions,
  switching,
  onPick,
}: {
  data: PhrasebookView;
  actions: Actions<PhrasebookView>;
  switching: string;
  onPick: (name: string) => Promise<void>;
}) {
  const slots = data.Slots ?? [];
  const personas = data.Personas ?? [];
  const generating = actions.pending === generateAction(data.Selected);
  const clearAction = `${baseAction(data.Selected)}/clear`;
  const groups = groupSlots(slots);

  return (
    <div className="space-y-2.5">
      <Section title="Persona" summary={data.Selected || "shipped defaults"} defaultOpen flush>
        <Lede>
          A phrase replaces the short confirmation one Tool speaks: one line, up to 80 characters. Every Persona keeps its own set; the
          shipped defaults are used when no Persona is active.
        </Lede>
        {personas.length ? (
          <div className="flex flex-wrap gap-1.5 px-3.5 pb-3">
            {personas.map((p) => {
              const on = p.Name === data.Selected;
              return (
                <button
                  key={p.Name}
                  type="button"
                  aria-pressed={on}
                  aria-label={`Show ${p.Name}${p.Active ? " (active persona)" : ""}, ${count(p.Count, "override")}`}
                  disabled={!!switching}
                  onClick={() => void onPick(p.Name)}
                  className={cn(
                    "flex min-h-9 items-center gap-1.5 border px-2.5 py-1 text-xs",
                    on ? "border-foreground bg-foreground text-background" : "hover:bg-surface-secondary",
                    switching === p.Name && "opacity-60",
                  )}
                >
                  <span className="max-w-40 truncate">{p.Name}</span>
                  {p.Active && <span className="border border-current px-1 text-[10px] tracking-wide uppercase">active</span>}
                  <span className="font-mono text-[10px] opacity-70">{p.Count}</span>
                </button>
              );
            })}
          </div>
        ) : (
          <Empty>No Personas are stored yet, so there is nothing to override.</Empty>
        )}

        {data.Selected ? (
          <div className="flex flex-wrap items-center gap-2 border-t px-3.5 py-2.5">
            <Button
              size="sm"
              variant="secondary"
              isDisabled={!data.HasGenerator}
              isPending={generating}
              onPress={() => void actions.run(generateAction(data.Selected))}
            >
              Generate from persona
            </Button>
            {data.Overridden > 0 && (
              <Button
                size="sm"
                variant="ghost"
                isPending={actions.pending === clearAction}
                onPress={() => void actions.run(clearAction)}
              >
                Clear all overrides
              </Button>
            )}
            <span className="text-[11px] text-subtle">
              {data.HasGenerator
                ? "Generating replaces every override for this persona and uses one model call."
                : "The Agent provider is not configured, so phrases cannot be generated."}
              {data.Overridden > 0 && " Clearing removes every override for this persona."}
            </span>
          </div>
        ) : (
          <div className="border-t bg-surface-secondary px-3.5 py-2.5 text-xs text-muted">
            No Persona is active. The phrases below are the shipped defaults, spoken as-is.
            {personas.length > 0 && " Select a Persona above to give it its own wording."}
          </div>
        )}
        {generating && (
          <p role="status" className="border-t px-3.5 py-2.5 text-xs text-muted">
            Generating a phrase for every Tool from this persona. This can take up to a minute; keep this page open.
          </p>
        )}
      </Section>

      <dl className="grid grid-cols-2 gap-px border bg-separator sm:grid-cols-4">
        <Stat label="Total" value={data.Total} />
        <Stat label="Overridden" value={data.Overridden} />
        <Stat label="Generated" value={data.Generated} />
        <Stat label="Manual" value={data.Manual} />
      </dl>

      {groups.length ? (
        groups.map(([group, rows]) => (
          <Section key={group} title={groupLabel(group)} summary={count(rows.length, "phrase")} defaultOpen flush>
            <ul className="divide-y divide-separator">
              {rows.map((slot) => (
                <PhraseRow key={`${data.Selected}:${slot.Tool}`} slot={slot} persona={data.Selected} actions={actions} />
              ))}
            </ul>
          </Section>
        ))
      ) : (
        <Section title="Phrases" defaultOpen>
          <Empty>No Tool has an overridable confirmation.</Empty>
        </Section>
      )}
    </div>
  );
}

const baseAction = (persona: string) => `/phrasebook/${encodeURIComponent(persona)}`;
const generateAction = (persona: string) => `${baseAction(persona)}/generate`;

/** One Tool: the phrase in an editable field, its source, and the actions for it. */
function PhraseRow({
  slot,
  persona,
  actions,
}: {
  slot: PhrasebookSlot;
  persona: string;
  actions: Actions<PhrasebookView>;
}) {
  const [draft, setDraft] = useState(slot.Phrase);
  // A save, a reset or a Persona switch all answer with a new view; follow it.
  useEffect(() => setDraft(slot.Phrase), [slot.Phrase]);
  const readOnly = !persona;
  const changed = !readOnly && draft !== slot.Phrase;
  const trimmed = draft.trim();
  const usable = trimmed !== "" && Array.from(trimmed).length <= 80;
  const saveAction = baseAction(persona);
  const resetAction = `${saveAction}/reset`;

  return (
    <li className="px-3.5 py-2.5">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-start">
        <div className="min-w-0 sm:w-56 sm:shrink-0">
          <div className="truncate font-mono text-xs">{slot.Tool}</div>
          <div className="truncate text-[11px] text-muted" title={slot.Description}>
            {slot.Description}
          </div>
        </div>
        <div className="min-w-0 flex-1 space-y-1">
          {readOnly ? (
            <p className="border bg-surface-secondary px-2.5 py-1.5 text-sm">{slot.Phrase}</p>
          ) : (
            <div className="flex items-center gap-2">
              <TextField label={`${slot.Tool} phrase`} value={draft} onChange={setDraft} className="min-w-0 flex-1" />
              <SourceBadge source={slot.Overridden ? "override" : "default"} label={sourceLabel(slot.Source)} />
            </div>
          )}
          {!readOnly && !usable && (
            <p className="text-[11px] text-danger">
              {trimmed ? "A phrase is one line, up to 80 characters." : "A phrase cannot be blank."}
            </p>
          )}
          {slot.Overridden && (
            <p className="text-[11px] text-subtle">
              Default: <span className="font-mono">{slot.Default}</span>
            </p>
          )}
        </div>
        {!readOnly && (changed || slot.Overridden) && (
          <div className="flex shrink-0 items-center gap-1">
            {changed && (
              <Button
                size="sm"
                isDisabled={!usable}
                isPending={actions.pending === saveAction}
                onPress={() => void actions.run(saveAction, { tool: slot.Tool, phrase: draft })}
              >
                Save
              </Button>
            )}
            {slot.Overridden && (
              <ResetButton
                label="Reset"
                isPending={actions.pending === resetAction}
                onPress={() => void actions.run(resetAction, { tool: slot.Tool })}
              />
            )}
          </div>
        )}
      </div>
    </li>
  );
}

function Stat({ label, value }: { label: string; value: number }) {
  return (
    <div className="flex flex-col-reverse bg-surface px-3.5 py-2.5">
      <dt className="text-[11px] text-muted">{label}</dt>
      <dd className="font-serif text-xl tabular-nums">{value}</dd>
    </div>
  );
}

function sourceLabel(source: string) {
  switch (source) {
    case "manual":
      return "Manual";
    case "agent":
      return "Agent";
    default:
      return "Default";
  }
}

/** Slots keep the server's group order, so the Map does too. */
function groupSlots(slots: PhrasebookSlot[]): [string, PhrasebookSlot[]][] {
  const groups = new Map<string, PhrasebookSlot[]>();
  for (const slot of slots) {
    const rows = groups.get(slot.Group) ?? [];
    rows.push(slot);
    groups.set(slot.Group, rows);
  }
  return [...groups];
}

const groupLabel = (group: string) => (group ? group.charAt(0).toUpperCase() + group.slice(1) : "Other");

const count = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
