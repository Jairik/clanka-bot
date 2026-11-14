// Write plumbing shared by every settings-like page. A write posts to the
// server-supplied action URL and the response is the whole refreshed view, so
// the page simply swaps it in: the server stays the only source of truth.
import { useEffect, useState, type ReactNode } from "react";
import { Button, Input, ListBox, Select, Switch, TextArea } from "@heroui/react";
import { RotateCcw } from "lucide-react";
import { postForm } from "./api";
import type { Flashable, Setting } from "./types";
import { Badge, Fold, Meta, SourceBadge, cn, foldOpen } from "./ui";
import { ChevronDown } from "lucide-react";

export type Actions<T> = {
  /** Action URL currently in flight, if any. */
  pending: string;
  run: (action: string, body?: Record<string, string> | FormData | Blob) => Promise<boolean>;
  flash: string;
  error: string;
  clear: () => void;
};

/**
 * Binds writes to one page view. `set` receives the refreshed view; the
 * flash/error are lifted out so they survive the next poll.
 */
export function useActions<T extends Flashable>(set: (v: T) => void): Actions<T> {
  const [pending, setPending] = useState("");
  const [flash, setFlash] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (!flash) return;
    const id = setTimeout(() => setFlash(""), 6000);
    return () => clearTimeout(id);
  }, [flash]);

  const run = async (action: string, body?: Record<string, string> | FormData | Blob) => {
    setPending(action);
    setError("");
    setFlash("");
    try {
      const res = await postForm<T>(action, body);
      if (res.view) {
        set(res.view);
        setFlash(res.view.Flash ?? "");
        setError(res.view.Error ?? "");
      } else if (!res.ok) {
        setError(res.text || `Request failed (${res.status}).`);
      }
      return res.ok;
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      return false;
    } finally {
      setPending("");
    }
  };
  return { pending, run, flash, error, clear: () => (setFlash(""), setError("")) };
}

// ---------- fields ----------

export function SelectField({
  label, value, options, onChange, className, isDisabled,
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange: (v: string) => void;
  className?: string;
  isDisabled?: boolean;
}) {
  return (
    <Select aria-label={label} className={cn("w-full sm:w-56", className)} value={value} onChange={(k) => onChange(String(k))} isDisabled={isDisabled}>
      <Select.Trigger className="h-9 text-sm">
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {options.map((o) => (
            <ListBox.Item key={o.value} id={o.value} textValue={o.label}>
              {o.label}
              <ListBox.ItemIndicator />
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

export function Toggle({ label, isSelected, onChange, isDisabled }: { label: string; isSelected: boolean; onChange: (v: boolean) => void; isDisabled?: boolean }) {
  return (
    <Switch isSelected={isSelected} onChange={onChange} aria-label={label} isDisabled={isDisabled}>
      <Switch.Control>
        <Switch.Thumb />
      </Switch.Control>
    </Switch>
  );
}

export function TextField({
  label, value, onChange, multiline, rows = 3, type = "text", placeholder, className, mono, inputMode, autoComplete,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  multiline?: boolean;
  rows?: number;
  type?: string;
  placeholder?: string;
  className?: string;
  mono?: boolean;
  inputMode?: "numeric" | "decimal" | "text";
  autoComplete?: string;
}) {
  const cls = cn("w-full text-sm", mono && "font-mono", className);
  return multiline ? (
    <TextArea aria-label={label} value={value} onChange={(e) => onChange(e.target.value)} rows={rows} placeholder={placeholder} className={cls} spellCheck={false} />
  ) : (
    <Input
      aria-label={label}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      type={type}
      placeholder={placeholder}
      className={cls}
      inputMode={inputMode}
      autoComplete={autoComplete}
      spellCheck={false}
    />
  );
}

/** A labelled block inside an editor. */
export function Field({ label, hint, children, className }: { label: string; hint?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <label className={cn("block space-y-1.5", className)}>
      <span className="text-xs text-muted">{label}</span>
      {children}
      {hint && <span className="block text-[11px] text-subtle">{hint}</span>}
    </label>
  );
}

export function ResetButton({ onPress, label, isPending }: { onPress: () => void; label: string; isPending?: boolean }) {
  return (
    <Button size="sm" variant="ghost" onPress={onPress} isPending={isPending} className="text-muted">
      <RotateCcw className="size-3.5" />
      {label}
    </Button>
  );
}

// ---------- one generic setting (Settings, Quiet hours, Ring kinds) ----------

/**
 * A setting row: label, effective value and source at a glance; the editor
 * folds open below. Bool settings save on toggle, everything else on Save.
 * `preview` renders the caller's view of the unsaved draft inside the fold
 * body; without it the row is unchanged.
 */
export function SettingRow<T extends Flashable>({
  s, actions, preview,
}: {
  s: Setting;
  actions: Actions<T>;
  preview?: (draft: string) => ReactNode;
}) {
  const [draft, setDraft] = useState(s.Edit);
  useEffect(() => setDraft(s.Edit), [s.Edit]);
  const saving = actions.pending === s.SaveAction;
  const resetting = actions.pending === s.ResetAction;

  if (s.FormKind === "bool") {
    return (
      <div className="flex min-h-14 items-center gap-3 px-3.5 py-2.5">
        <SettingLabel s={s} />
        <Toggle label={s.Label} isSelected={s.Bool} isDisabled={saving} onChange={(v) => void actions.run(s.SaveAction, { value: String(v) })} />
        {s.Override && <ResetButton label="Reset" isPending={resetting} onPress={() => void actions.run(s.ResetAction)} />}
      </div>
    );
  }
  const kind = s.FormKind;
  return (
    <Fold
      triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2.5 hover:bg-surface-secondary"
      bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
      trigger={
        <>
          <SettingLabel s={s} />
          <span className="hidden max-w-[40%] truncate text-right font-mono text-xs sm:block">{s.Display || "—"}</span>
          <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
        </>
      }
    >
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          void actions.run(s.SaveAction, { value: draft });
        }}
      >
        <Meta items={[["Effective", <span className="font-mono">{s.Display || "—"}</span>]]} className="sm:hidden" />
        <TextField
          label={s.Label}
          value={draft}
          onChange={setDraft}
          multiline={kind === "lines"}
          rows={Math.min(8, Math.max(3, draft.split("\n").length + 1))}
          type={kind === "number" || kind === "decimal" ? "number" : "text"}
          inputMode={kind === "number" ? "numeric" : kind === "decimal" ? "decimal" : undefined}
          placeholder={kind === "duration" ? "e.g. 9m0s" : undefined}
          mono={kind !== "text" && kind !== "lines"}
        />
        {kind === "lines" && <p className="text-[11px] text-subtle">One entry per line; blank lines are ignored.</p>}
        {preview?.(draft)}
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" type="submit" isPending={saving} isDisabled={draft === s.Edit && !s.Restart}>
            Save
          </Button>
          {s.Override ? (
            <ResetButton label={`Reset to ${s.LowerLabel}`} isPending={resetting} onPress={() => void actions.run(s.ResetAction)} />
          ) : (
            <span className="text-xs text-subtle">No override; showing the {s.SourceLabel} value.</span>
          )}
        </div>
      </form>
    </Fold>
  );
}

function SettingLabel({ s }: { s: Setting }) {
  return (
    <div className="min-w-0 flex-1">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-sm">{s.Label}</span>
        <SourceBadge source={s.Source} label={s.SourceLabel} />
        {s.Restart && <Badge tone="warn">Restart required</Badge>}
      </div>
      <div className="truncate font-mono text-[11px] text-subtle">{s.Key}</div>
      {s.Description && <div className="mt-0.5 text-xs text-muted">{s.Description}</div>}
    </div>
  );
}
