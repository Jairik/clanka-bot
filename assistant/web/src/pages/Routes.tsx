// Routes: the read-only explanation of how a request is answered, and which
// Tools each path may choose. Counts are live settings, not code defaults.
import { useState, type ReactNode } from "react";
import { Button } from "@heroui/react";
import { usePageData } from "../api";
import { TextField } from "../forms";
import { PageFrame } from "../shell";
import type { RouteRow, RoutesView } from "../types";
import { Badge, Empty, Lede, Notice, Section } from "../ui";

export function RoutesPage() {
  const { data, error } = usePageData("routes");
  const [filter, setFilter] = useState("");

  return (
    <PageFrame id="routes">
      <Notice error={error || (data?.Error ?? "")} />
      {data && !data.HasRegistry && (
        <Section title="The Tool registry is not configured" defaultOpen>
          <Empty>This Dashboard was built without the Tool registry, so there is no routing to show.</Empty>
        </Section>
      )}
      {data?.HasRegistry && <RoutesBody data={data} filter={filter} onFilter={setFilter} />}
    </PageFrame>
  );
}

const LEGEND = [
  ["Fast path", "Deterministic: a matching control phrase answers without the model."],
  ["Agent only", "The Fast path never chooses it; only the Agent calls it."],
  ["Pi only", "Runs on the Pi; the request and its data never leave the house."],
  ["Hermes", "Read or act, not Pi-only, not Confirm: part of the surface Hermes would get."],
];

const STEPS = [
  "Pending Confirmation",
  "Request prefixes",
  "Control phrase / Focus intent",
  "Semantic Fast path",
  "Agent path",
  "Hermes for “ask Hermes” / delegated jobs",
];

const PERMISSIONS: [string, string][] = [
  ["read", "Read"],
  ["act", "Act"],
  ["confirm", "Confirm"],
];

function RoutesBody({ data, filter, onFilter }: { data: RoutesView; filter: string; onFilter: (v: string) => void }) {
  const rows = data.Rows ?? [];
  const q = filter.trim().toLowerCase();
  const shown = q
    ? rows.filter((r) => [r.Name, r.Description, r.Permission].some((v) => v.toLowerCase().includes(q)))
    : rows;
  const totals = data.Totals;

  return (
    <div className="space-y-2.5">
      <div className="grid gap-2 border bg-surface p-3.5 sm:grid-cols-2">
        {LEGEND.map(([badge, text]) => (
          <div key={badge} className="flex items-start gap-2">
            <Badge className="mt-0.5 shrink-0">{badge}</Badge>
            <p className="text-xs text-muted">{text}</p>
          </div>
        ))}
      </div>

      <dl className="grid grid-cols-3 gap-px border bg-separator sm:grid-cols-6">
        <Stat label="Tools" value={totals.Tools} />
        <Stat label="Fast path" value={totals.FastPath} />
        <Stat label="Agent only" value={totals.AgentOnly} />
        <Stat label="Pi only" value={totals.LocalOnly} />
        <Stat label="Confirm" value={totals.Confirm} />
        <Stat label="Hermes" value={totals.Hermes} />
      </dl>

      <Section title="Routing order" defaultOpen flush>
        <Lede>A request walks these in order and stops at the first step that answers.</Lede>
        <ol className="flex flex-wrap items-center gap-x-1.5 gap-y-1.5 px-3.5 py-3 text-xs">
          {STEPS.map((step, i) => (
            <li key={step} className="flex items-center gap-1.5">
              <span className="flex size-5 shrink-0 items-center justify-center border font-mono text-[10px] text-muted">{i + 1}</span>
              <span>{step}</span>
              {i < STEPS.length - 1 && (
                <span aria-hidden className="text-subtle">
                  →
                </span>
              )}
            </li>
          ))}
        </ol>
        {totals.Hermes > 0 && (
          <p className="px-3.5 pb-3 text-[11px] text-subtle">
            Hermes is not installed in this build, so the last step and the {totals.Hermes} Tools it would see are unused today.
          </p>
        )}
      </Section>

      <Section title="Agent wording" summary={`${data.Prefixes?.length ?? 0} prefixes`} flush>
        <Lede>These are the live settings the Agent path and the Unavailable reply read.</Lede>
        <dl className="divide-y divide-separator text-xs">
          <WordingRow label="Request prefixes">
            {data.Prefixes?.length ? (
              <span className="flex flex-wrap justify-end gap-1">
                {data.Prefixes.map((p) => (
                  <Badge key={p} className="font-mono">
                    {p}
                  </Badge>
                ))}
              </span>
            ) : (
              <span className="text-subtle">none</span>
            )}
          </WordingRow>
          <WordingRow label="Filler">
            {data.Filler ? <span className="text-right">{data.Filler}</span> : <span className="text-subtle">none</span>}
          </WordingRow>
          <WordingRow label="Unavailable">
            {data.Unavailable ? <span className="text-right">{data.Unavailable}</span> : <span className="text-subtle">none</span>}
          </WordingRow>
        </dl>
      </Section>

      <Section
        title="Tools"
        summary={q ? `${shown.length} of ${rows.length}` : count(rows.length, "tool")}
        defaultOpen
        flush
      >
        <div className="flex flex-wrap items-end gap-2 px-3.5 py-3">
          <TextField label="Filter tools" value={filter} onChange={onFilter} placeholder="Name, description or permission" className="sm:w-72" />
          {filter && (
            <Button size="sm" variant="ghost" onPress={() => onFilter("")}>
              Clear
            </Button>
          )}
        </div>
        {shown.length ? (
          PERMISSIONS.map(([key, label]) => {
            const group = shown.filter((r) => r.Permission === key);
            if (!group.length) return null;
            return (
              <div key={key} className="border-t">
                <h3 className="flex items-baseline gap-2 bg-surface-secondary px-3.5 py-1.5 text-[11px] tracking-wide text-muted uppercase">
                  {label}
                  <span className="normal-case">{count(group.length, "tool")}</span>
                </h3>
                <ul className="divide-y divide-separator">
                  {group.map((r) => (
                    <RouteRowItem key={r.Name} row={r} />
                  ))}
                </ul>
              </div>
            );
          })
        ) : (
          <Empty>No Tool matches “{filter.trim()}”.</Empty>
        )}
      </Section>
    </div>
  );
}

function RouteRowItem({ row }: { row: RouteRow }) {
  return (
    <li className="flex flex-col gap-1.5 px-3.5 py-2.5 sm:flex-row sm:items-start sm:gap-3">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="font-mono text-sm">{row.Name}</span>
          <Badge>{row.Permission}</Badge>
          {!row.Enabled && <Badge tone="warn">Disabled</Badge>}
        </div>
        <p className="mt-0.5 truncate text-xs text-muted" title={row.Description}>
          {row.Description}
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-1.5 sm:max-w-[55%] sm:justify-end">
        {row.FastPath && <Badge>Fast path</Badge>}
        {row.AgentOnly && <Badge>Agent only</Badge>}
        {row.LocalOnly && <Badge>Pi only</Badge>}
        {row.Confirm && <Badge>Confirm</Badge>}
        {row.Hermes && <Badge>Hermes</Badge>}
        <span className="text-[11px] text-subtle tabular-nums">
          {count(row.ControlPhrases, "phrase")} · {count(row.Examples, "example")}
        </span>
      </div>
    </li>
  );
}

function WordingRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 px-3.5 py-2.5">
      <dt className="shrink-0 text-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: number }) {
  return (
    <div className="flex flex-col-reverse bg-surface px-3 py-2.5">
      <dt className="text-[11px] text-muted">{label}</dt>
      <dd className="font-serif text-xl tabular-nums">{value}</dd>
    </div>
  );
}

const count = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
