// Hermes Tool recipes: named, described presets over one vetted read-only Tool,
// with optional fixed JSON arguments. The server owns the vetted list and
// validates every recipe; this section only lists, drafts and submits.
import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Switch } from "@heroui/react";
import { ChevronDown } from "lucide-react";
import { HttpError, getJSON, postForm } from "../api";
import { Field, SelectField, TextField } from "../forms";
import { Badge, Empty, Fold, InitialRead, Lede, Notice, Section, cn, foldOpen } from "../ui";

export type HermesAvailableTool = { name: string; description: string };
export type HermesRecipe = {
  name: string;
  description: string;
  base_tool: string;
  arguments: Record<string, unknown> | null;
  enabled: boolean;
};
export type HermesToolsView = { available: HermesAvailableTool[] | null; recipes: HermesRecipe[] | null };

export const TOOLS_PATH = "/api/ui/hermes/tools";
const JSON_INIT: RequestInit = { headers: { "Content-Type": "application/json", Accept: "application/json" } };
// These mirror the assistant's recipe rules (hermesadapter/recipes.go): the
// name becomes the Tool "recipe.<name>", so it follows the runtime's
// snake_case name part (tool.WireName) with no "__", and is capped at 56 so
// "recipe__<name>" fits a 64-character function name.
const NAME_RE = /^[a-z][a-z0-9_]*$/;
const NAME_MAX = 56;
const DESCRIPTION_MAX = 500; // runes, after trimming
const ARGUMENTS_MAX_BYTES = 8 << 10; // of the compact JSON object the server stores
// Go's unicode.IsControl: the C0 and C1 control characters plus DEL.
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/;

const deleteAction = (name: string) => `${TOOLS_PATH}/${encodeURIComponent(name)}/delete`;

/** The assistant answers every refusal as {"error": "..."}; anything else falls back to text. */
export function apiError(body: unknown): string {
  let v = body;
  if (typeof v === "string") {
    try {
      v = JSON.parse(v);
    } catch {
      return "";
    }
  }
  if (!v || typeof v !== "object" || Array.isArray(v)) return "";
  const msg = (v as { error?: unknown }).error;
  return typeof msg === "string" ? msg.trim() : "";
}

/** Pull an error message out of a refused write, whatever shape it answered with. */
function refusal(status: number, view: unknown, text: string): string {
  return apiError(view) || apiError(text) || text || `Request failed (${status}).`;
}

/** A served view always carries both lists, even when they are empty. */
const isView = (v: unknown): v is HermesToolsView =>
  !!v &&
  typeof v === "object" &&
  Array.isArray((v as HermesToolsView).available) &&
  Array.isArray((v as HermesToolsView).recipes);

/** Bytes of the arguments as Go re-encodes them: compact UTF-8, with <, >, & and U+2028/9 escaped. */
export function argumentsBytes(value: Record<string, unknown>): number {
  const text = JSON.stringify(value);
  const escaped = text.match(/[<>&]/g)?.length ?? 0; // "<" is 6 bytes, not 1
  const separators = text.match(/[\u2028\u2029]/g)?.length ?? 0; // "\u2028" is 6 bytes, not 3
  return new TextEncoder().encode(text).length + 5 * escaped + 3 * separators;
}

/** Loads the recipe view and runs writes against it. Newest write wins. */
function useTools() {
  const [data, setData] = useState<HermesToolsView | null>(null);
  const [loadError, setLoadError] = useState("");
  const [missing, setMissing] = useState(false);
  const [pending, setPending] = useState("");
  const [error, setError] = useState("");
  const [flash, setFlash] = useState("");
  const seq = useRef(0);

  const load = useCallback(async () => {
    setLoadError("");
    try {
      setData(await getJSON<HermesToolsView>(TOOLS_PATH));
      setMissing(false);
    } catch (e) {
      // An assistant without the recipe routes answers 404: say so plainly
      // instead of offering a retry that cannot succeed.
      if (e instanceof HttpError && e.status === 404) setMissing(true);
      else {
        const text = e instanceof Error ? e.message : String(e);
        setLoadError(apiError(text) || text);
      }
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!flash) return;
    const id = setTimeout(() => setFlash(""), 6000);
    return () => clearTimeout(id);
  }, [flash]);

  /** `key` names the pending control; returns whether the write was accepted. */
  const run = async (action: string, key: string, body: string | undefined, done: string) => {
    const mine = ++seq.current;
    setPending(key);
    setError("");
    setFlash("");
    try {
      const res = await postForm<unknown>(action, body, body === undefined ? undefined : JSON_INIT);
      if (mine !== seq.current) return res.ok;
      if (res.ok && isView(res.view)) {
        setData(res.view);
        setFlash(done);
      } else if (res.ok) {
        setError("The assistant answered without the recipe list. Reload to see the current state.");
      } else {
        if (isView(res.view)) setData(res.view);
        setError(refusal(res.status, res.view, res.text));
      }
      return res.ok;
    } catch (e) {
      if (mine === seq.current) setError(e instanceof Error ? e.message : String(e));
      return false;
    } finally {
      if (mine === seq.current) setPending("");
    }
  };

  return { data, loadError, missing, load, pending, error, flash, run, clear: () => (setError(""), setFlash("")) };
}

type Tools = ReturnType<typeof useTools>;

type Draft = { name: string; description: string; base_tool: string; arguments: string; enabled: boolean };

const draftOf = (r: HermesRecipe): Draft => ({
  name: r.name,
  description: r.description,
  base_tool: r.base_tool,
  arguments: r.arguments && Object.keys(r.arguments).length ? JSON.stringify(r.arguments, null, 2) : "",
  enabled: r.enabled,
});

/** Parse the fixed-arguments box: blank is no arguments; otherwise one JSON object. */
export function parseArguments(text: string): { value: Record<string, unknown> | null; error: string } {
  if (!text.trim()) return { value: null, error: "" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { value: null, error: "Arguments must be valid JSON." };
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { value: null, error: "Arguments must be a JSON object, like {\"city\": \"Austin\"}." };
  }
  return { value: parsed as Record<string, unknown>, error: "" };
}

/** Client-side checks that save a round trip; the server stays authoritative. */
export function validateDraft(
  d: Draft,
  ctx: { creating: boolean; recipes: HermesRecipe[]; available: HermesAvailableTool[] },
): Partial<Record<keyof Draft, string>> {
  const errs: Partial<Record<keyof Draft, string>> = {};
  const name = d.name.trim();
  if (ctx.creating) {
    if (!name) errs.name = "Name is required.";
    else if (name.length > NAME_MAX) errs.name = `Name must be ${NAME_MAX} characters or fewer.`;
    else if (!NAME_RE.test(name)) errs.name = "Start with a lowercase letter, then use lowercase letters, digits or underscores.";
    else if (name.includes("__")) errs.name = "Use single underscores only.";
    else if (ctx.recipes.some((r) => r.name === name)) errs.name = "A recipe with this name already exists.";
  }
  const description = d.description.trim();
  if (!description) errs.description = "Description is required.";
  else if (CONTROL_RE.test(description)) errs.description = "Keep the description on one line, without control characters.";
  else if ([...description].length > DESCRIPTION_MAX) errs.description = `Description must be ${DESCRIPTION_MAX} characters or fewer.`;
  if (!d.base_tool) errs.base_tool = "Choose a vetted Tool.";
  else if (!ctx.available.some((t) => t.name === d.base_tool)) errs.base_tool = "This Tool is no longer available.";
  const args = parseArguments(d.arguments);
  if (args.error) errs.arguments = args.error;
  else if (args.value && argumentsBytes(args.value) > ARGUMENTS_MAX_BYTES) {
    errs.arguments = `Arguments must fit in ${ARGUMENTS_MAX_BYTES} bytes of JSON.`;
  }
  return errs;
}

const payload = (d: Draft): string =>
  JSON.stringify({
    name: d.name.trim(),
    description: d.description.trim(),
    base_tool: d.base_tool,
    arguments: parseArguments(d.arguments).value ?? {},
    enabled: d.enabled,
  });

export function HermesToolsSection() {
  const tools = useTools();
  const { data } = tools;
  const available = data?.available ?? [];
  const recipes = data?.recipes ?? [];
  const enabledCount = recipes.filter((r) => r.enabled).length;

  if (tools.missing) {
    return (
      <Section title="Tool recipes" defaultOpen>
        <Empty>This assistant does not serve Tool recipes yet.</Empty>
      </Section>
    );
  }

  return (
    <Section
      title="Tool recipes"
      summary={data ? `${enabledCount} of ${recipes.length} on · ${available.length} vetted` : undefined}
      defaultOpen
      flush
    >
      {!data ? (
        <InitialRead className="px-3.5" error={tools.loadError} retry={tools.load} />
      ) : (
        <>
          <Lede>
            A recipe gives Hermes a named shortcut over one vetted read-only Tool, with any fixed arguments filled in. Only the
            Tools below can back a recipe.
          </Lede>
          <div className="px-3.5 pt-2">
            <Notice error={tools.error} flash={tools.flash} onDismiss={tools.clear} />
          </div>

          <h3 className="px-3.5 pt-1 pb-1 text-xs text-muted">Vetted read-only Tools</h3>
          {available.length ? (
            <ul className="divide-y divide-separator border-y" aria-label="Vetted read-only Tools">
              {available.map((t) => (
                <li key={t.name} className="flex min-h-10 items-center gap-3 px-3.5 py-1.5">
                  <span className="shrink-0 font-mono text-xs">{t.name}</span>
                  <span className="min-w-0 flex-1 truncate text-[11px] text-muted">{t.description}</span>
                </li>
              ))}
            </ul>
          ) : (
            <Empty>No vetted read-only Tool is available, so no recipe can be added.</Empty>
          )}

          <h3 className="px-3.5 pt-3 pb-1 text-xs text-muted">Saved recipes</h3>
          {recipes.length ? (
            <ul className="divide-y divide-separator border-t" aria-label="Saved recipes">
              {recipes.map((r) => (
                <li key={r.name}>
                  <RecipeRow recipe={r} tools={tools} available={available} recipes={recipes} />
                </li>
              ))}
            </ul>
          ) : (
            <Empty>No recipe saved yet.</Empty>
          )}

          {available.length > 0 && (
            <div className="border-t">
              <Fold
                triggerClassName="flex min-h-12 items-center gap-3 px-3.5 py-2.5 text-sm hover:bg-surface-secondary"
                bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
                trigger={
                  <>
                    <span className="flex-1">Add a recipe</span>
                    <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
                  </>
                }
              >
                <RecipeForm tools={tools} available={available} recipes={recipes} />
              </Fold>
            </div>
          )}
        </>
      )}
    </Section>
  );
}

function RecipeRow({
  recipe, tools, available, recipes,
}: { recipe: HermesRecipe; tools: Tools; available: HermesAvailableTool[]; recipes: HermesRecipe[] }) {
  const toggleKey = `toggle:${recipe.name}`;
  const busy = tools.pending !== "";
  return (
    <div>
      <div className="flex min-h-14 items-center gap-3 px-3.5 py-2.5">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="truncate font-mono text-sm">{recipe.name}</span>
            <Badge className="font-mono">{recipe.base_tool}</Badge>
            {!recipe.enabled && <Badge>off</Badge>}
          </div>
          <div className="mt-0.5 truncate text-[11px] text-muted">{recipe.description}</div>
        </div>
        <Switch
          isSelected={recipe.enabled}
          isDisabled={busy}
          aria-label={`Enable recipe ${recipe.name}`}
          onChange={(on) =>
            void tools.run(
              TOOLS_PATH,
              toggleKey,
              payload({ ...draftOf(recipe), enabled: on }),
              `${recipe.name} ${on ? "enabled" : "disabled"}.`,
            )
          }
        >
          <Switch.Content className="flex items-center">
            <Switch.Control>
              <Switch.Thumb />
            </Switch.Control>
            <span className="sr-only">Enable recipe {recipe.name}</span>
          </Switch.Content>
        </Switch>
      </div>
      <Fold
        triggerClassName="flex items-center gap-1.5 px-3.5 pb-2 text-xs text-muted hover:text-foreground"
        bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
        trigger={
          <>
            <span>
              Edit <span className="sr-only">recipe {recipe.name}</span>
            </span>
            <ChevronDown className={cn("size-3.5", foldOpen)} />
          </>
        }
      >
        <RecipeForm tools={tools} available={available} recipes={recipes} recipe={recipe} />
      </Fold>
    </div>
  );
}

/** Create (no `recipe`) or edit one recipe. The name is fixed once saved. */
function RecipeForm({
  tools, available, recipes, recipe,
}: { tools: Tools; available: HermesAvailableTool[]; recipes: HermesRecipe[]; recipe?: HermesRecipe }) {
  const creating = !recipe;
  const blank: Draft = { name: "", description: "", base_tool: available[0]?.name ?? "", arguments: "", enabled: true };
  const [draft, setDraft] = useState<Draft>(recipe ? draftOf(recipe) : blank);
  const [shown, setShown] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  // Follow the server's copy after a save elsewhere (toggle, another tab).
  const serverKey = recipe ? JSON.stringify(recipe) : "";
  useEffect(() => {
    if (recipe) setDraft(draftOf(recipe));
  }, [serverKey]); // recipe is read through serverKey

  const errs = validateDraft(draft, { creating, recipes, available });
  const invalid = Object.keys(errs).length > 0;
  const saveKey = creating ? "create" : `save:${recipe.name}`;
  const delKey = recipe ? `delete:${recipe.name}` : "";
  const busy = tools.pending !== "";
  const up = <K extends keyof Draft>(k: K) => (v: Draft[K]) => setDraft((d) => ({ ...d, [k]: v }));
  const show = (k: keyof Draft) => (shown ? errs[k] : undefined);

  const save = async () => {
    setShown(true);
    if (invalid) return;
    const name = draft.name.trim();
    const ok = await tools.run(TOOLS_PATH, saveKey, payload(draft), creating ? `${name} added.` : `${name} saved.`);
    if (ok && creating) {
      setDraft(blank);
      setShown(false);
    }
  };

  return (
    <form
      className="space-y-3"
      noValidate
      autoComplete="off"
      aria-label={creating ? "New recipe" : `Edit recipe ${recipe.name}`}
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      {creating ? (
        <Field label="Name" hint={<FieldHint error={show("name")} hint="Lowercase letters, digits and single underscores, e.g. home_weather. It cannot be changed later." />}>
          <TextField label="Name" mono value={draft.name} onChange={up("name")} placeholder="home_weather" />
        </Field>
      ) : (
        <div className="space-y-1">
          <div className="text-xs text-muted">Name</div>
          <div className="font-mono text-sm">{recipe.name}</div>
          <p className="text-[11px] text-subtle">A saved name is fixed. Delete and add again to rename.</p>
        </div>
      )}

      <Field label="Description" hint={<FieldHint error={show("description")} hint="Hermes reads this to decide when to use the recipe." />}>
        <TextField label="Description" value={draft.description} onChange={up("description")} placeholder="Current weather at home" />
      </Field>

      <div className="space-y-1.5">
        <span className="text-xs text-muted">Vetted Tool</span>
        <SelectField
          label="Vetted Tool"
          value={draft.base_tool}
          onChange={up("base_tool")}
          options={available.map((t) => ({ value: t.name, label: t.name }))}
          className="sm:w-72"
        />
        <FieldHint error={show("base_tool")}
          hint={available.find((t) => t.name === draft.base_tool)?.description}
        />
      </div>

      <Field
        label="Fixed arguments (optional JSON)"
        hint={<FieldHint error={show("arguments")} hint="A JSON object passed to the Tool on every call. Leave blank for none." />}
      >
        <TextField
          label="Fixed arguments (optional JSON)"
          multiline
          mono
          rows={Math.min(8, Math.max(3, draft.arguments.split("\n").length + 1))}
          value={draft.arguments}
          onChange={up("arguments")}
          placeholder={'{"city": "Austin"}'}
        />
      </Field>

      {creating && (
        <Switch isSelected={draft.enabled} onChange={up("enabled")} aria-label="Enable when saved">
          <Switch.Content className="flex items-center gap-3">
            <Switch.Control>
              <Switch.Thumb />
            </Switch.Control>
            <span className="text-sm">Enable when saved</span>
          </Switch.Content>
        </Switch>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" type="submit" isPending={tools.pending === saveKey} isDisabled={busy && tools.pending !== saveKey}>
          {creating ? "Add recipe" : "Save"}
        </Button>
        {recipe &&
          (confirmDelete ? (
            <>
              <Button
                size="sm"
                variant="danger-soft"
                isPending={tools.pending === delKey}
                isDisabled={busy && tools.pending !== delKey}
                onPress={() => void tools.run(deleteAction(recipe.name), delKey, undefined, `${recipe.name} deleted.`)}
                aria-label={`Confirm delete recipe ${recipe.name}`}
              >
                Confirm delete
              </Button>
              <Button size="sm" variant="ghost" onPress={() => setConfirmDelete(false)}>
                Keep
              </Button>
            </>
          ) : (
            <Button size="sm" variant="ghost" isDisabled={busy} onPress={() => setConfirmDelete(true)} aria-label={`Delete recipe ${recipe.name}`}>
              Delete
            </Button>
          ))}
        {shown && invalid && (
          <span role="alert" className="text-[11px] text-danger">
            Fix the highlighted fields to save.
          </span>
        )}
      </div>
    </form>
  );
}

function FieldHint({ error, hint }: { error?: string; hint?: string }) {
  if (error) return <span className="text-danger">{error}</span>;
  return hint ? <span>{hint}</span> : null;
}
