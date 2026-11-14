// Thin helpers over HeroUI v3 (React Aria underneath). Anything that is just a
// HeroUI component is imported from @heroui/react at the call site; these exist
// only where the dashboard repeats the same composition.
import { Chip, Disclosure, Focusable, Slider, Tooltip } from "@heroui/react";
import { ChevronRight } from "lucide-react";
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
import type { DOMAttributes, ReactElement, ReactNode } from "react";

type FocusableElement = Element;

type TipPlacement = "top" | "bottom" | "left" | "right" | "bottom end" | "bottom start";

/**
 * A short hover/focus label for a control. The child is the trigger itself, so
 * no wrapper element (and no stray button role) is added: a React Aria
 * component such as a HeroUI Button takes the trigger props directly, and a
 * plain element (a link, a span with tabIndex) is passed through Focusable.
 * Without `label`, the child's aria-label is shown, so an icon button's name
 * and its tooltip cannot drift apart.
 */
export function Tip({ label, hint, children, placement = "bottom" }: {
  label?: ReactNode;
  /** A second, muted line under the label. */
  hint?: ReactNode;
  children: ReactElement<{ "aria-label"?: string }>;
  placement?: TipPlacement;
}) {
  const text = label ?? children.props["aria-label"];
  if (!text) return children;
  const trigger =
    typeof children.type === "string" ? <Focusable>{children as ReactElement<DOMAttributes<FocusableElement>, string>}</Focusable> : children;
  return (
    <Tooltip delay={350} closeDelay={0}>
      {trigger}
      <Tooltip.Content placement={placement} className="px-2.5 py-1.5">
        <span className="block">{text}</span>
        {hint && <span className="mt-0.5 block max-w-56 text-muted">{hint}</span>}
      </Tooltip.Content>
    </Tooltip>
  );
}

/** Status color. Everything else in the UI is grayscale. */
export type Tone = "ok" | "warn" | "bad" | "neutral";

export const cn = (...c: ClassValue[]) => twMerge(clsx(c));

const CHIP_COLOR = { ok: "success", warn: "warning", bad: "danger", neutral: "default" } as const;
/** Small status/metadata pill. Gray unless it carries a status. */
export function Badge({ tone = "neutral", className, children }: { tone?: Tone; className?: string; children: ReactNode }) {
  return (
    <Chip size="sm" variant="soft" color={CHIP_COLOR[tone]} className={cn("gap-1 text-[11px]", className)}>
      {children}
    </Chip>
  );
}

export const TONE_BG: Record<Tone, string> = { ok: "bg-success", warn: "bg-warning", bad: "bg-danger", neutral: "bg-muted/50" };
export function Dot({ tone = "neutral", pulse }: { tone?: Tone; pulse?: boolean }) {
  return (
    <span className="relative inline-flex size-2 shrink-0">
      {pulse && <span className={cn("absolute inset-0 animate-ping rounded-full opacity-60", TONE_BG[tone])} />}
      <span className={cn("relative size-2 rounded-full", TONE_BG[tone])} />
    </span>
  );
}

/** The collapsible building block used everywhere: a header that summarizes its body. */
export function Section({
  title, summary, right, tone, defaultOpen, children, className, flush,
}: {
  title: ReactNode;
  summary?: ReactNode;
  right?: ReactNode;
  tone?: Tone;
  defaultOpen?: boolean;
  children: ReactNode;
  className?: string;
  flush?: boolean;
}) {
  return (
    <Disclosure defaultExpanded={defaultOpen} className={cn("group/sec rounded-xl border bg-surface", className)}>
      <Disclosure.Heading level={2}>
        <Disclosure.Trigger className="flex min-h-12 w-full items-center gap-2.5 rounded-xl px-3.5 py-2.5 text-left hover:bg-surface-secondary">
          <ChevronRight className="size-4 shrink-0 text-muted transition-transform group-data-[expanded=true]/sec:rotate-90" />
          {tone && <Dot tone={tone} />}
          <span className="min-w-0 truncate text-sm font-semibold">{title}</span>
          {summary && <span className="ml-auto min-w-0 truncate text-xs text-muted">{summary}</span>}
          {right}
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className={cn("border-t", !flush && "p-1.5")}>{children}</Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

/** Row-level disclosure: one-line trigger, detail below. `group/f` exposes the state. */
export function Fold({ trigger, children, className, triggerClassName, bodyClassName }: {
  trigger: ReactNode;
  children: ReactNode;
  className?: string;
  triggerClassName?: string;
  bodyClassName?: string;
}) {
  return (
    <Disclosure className={cn("group/f", className)}>
      <Disclosure.Heading>
        <Disclosure.Trigger className={cn("w-full text-left", triggerClassName)}>{trigger}</Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className={bodyClassName}>{children}</Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}
/** Chevron that rotates when its enclosing Fold is open. */
export const foldOpen = "transition-transform group-data-[expanded=true]/f:rotate-180";

/** HeroUI slider, single value, track-only (labels are rendered by the caller). */
export function Range({ value, onChange, step = 5, className, label }: {
  value: number;
  onChange: (v: number) => void;
  step?: number;
  className?: string;
  label: string;
}) {
  return (
    <Slider aria-label={label} value={value} onChange={(v) => onChange(v as number)} step={step} maxValue={100} className={cn("w-full", className)}>
      <Slider.Track>
        <Slider.Fill />
        <Slider.Thumb />
      </Slider.Track>
    </Slider>
  );
}

/** Page-level result of the last write. Flash fades on its own; errors stay until the next action. */
export function Notice({ flash, error, onDismiss }: { flash?: string; error?: string; onDismiss?: () => void }) {
  if (!flash && !error) return null;
  return (
    <div
      role={error ? "alert" : "status"}
      className={cn(
        "mb-4 flex items-start gap-3 border px-3.5 py-2.5 text-sm",
        error ? "border-danger/40 bg-danger/8 text-danger" : "bg-surface-secondary",
      )}
    >
      <Dot tone={error ? "bad" : "ok"} />
      <span className="-mt-1 min-w-0 flex-1 break-words">{error || flash}</span>
      {onDismiss && (
        <button onClick={onDismiss} className="-mt-0.5 text-xs text-muted hover:text-foreground">
          Dismiss
        </button>
      )}
    </div>
  );
}

/** Where a value came from: an override is the only one worth drawing attention to. */
export function SourceBadge({ source, label }: { source: string; label: string }) {
  return (
    <Badge className={cn(source === "override" && "bg-foreground text-background")} tone="neutral">
      {label}
    </Badge>
  );
}

/** Label/value pairs in one wrapping line. */
export function Meta({ items, className }: { items: [string, ReactNode][]; className?: string }) {
  const shown = items.filter(([, v]) => v !== "" && v != null && v !== false);
  if (!shown.length) return null;
  return (
    <dl className={cn("flex flex-wrap gap-x-4 gap-y-1 text-xs", className)}>
      {shown.map(([k, v]) => (
        <div key={k} className="flex min-w-0 gap-1.5">
          <dt className="text-muted">{k}</dt>
          <dd className="min-w-0 break-words">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="px-3.5 py-6 text-center text-sm text-muted">{children}</p>;
}

/** Explanatory copy at the top of a section body. */
export function Lede({ children }: { children: ReactNode }) {
  return <p className="px-3.5 pt-3 pb-1 text-xs leading-relaxed text-muted">{children}</p>;
}
