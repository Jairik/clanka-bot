import { useState } from "react";
import { Button } from "@heroui/react";
import { ChevronDown } from "lucide-react";
import { usePageData } from "../api";
import { TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { Secret, SecretsView } from "../types";
import { Dot, Empty, Fold, Lede, Meta, Notice, Section, SourceBadge, cn, foldOpen } from "../ui";

export function SecretsPage() {
  const { data, error, set } = usePageData("secrets");
  const actions = useActions<SecretsView>(set);
  const secrets = data?.Secrets ?? [];
  const unset = secrets.filter((s) => !s.Set).length;
  return (
    <PageFrame id="secrets">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasSecrets && (
        <Section title="Secrets are not configured" defaultOpen>
          <Empty>This Dashboard was built without the secrets service, so there is nothing to edit.</Empty>
        </Section>
      )}
      {data?.HasSecrets && (
        <Section title="Declared secrets" tone={unset ? "warn" : "ok"} summary={unset ? `${unset} not set` : `${secrets.length} set`} defaultOpen flush>
          <Lede>
            Values are write-only: only whether one is set, its last four characters and when it changed are shown. Replacing takes effect on the next request.
            Clearing removes the stored value; a <code>.env</code> value with the same name then applies again.
          </Lede>
          {secrets.length ? (
            <ul className="divide-y divide-separator">
              {secrets.map((s) => (
                <SecretRow key={s.Name} s={s} actions={actions} />
              ))}
            </ul>
          ) : (
            <Empty>No secret is declared.</Empty>
          )}
        </Section>
      )}
    </PageFrame>
  );
}

function SecretRow({ s, actions }: { s: Secret; actions: Actions<SecretsView> }) {
  const [value, setValue] = useState("");
  return (
    <li id={s.Anchor}>
      <Fold
        triggerClassName="flex min-h-14 items-center gap-3 px-3.5 py-2.5 hover:bg-surface-secondary"
        bodyClassName="border-t border-dashed bg-surface-secondary px-3.5 py-3"
        trigger={
          <>
            <Dot tone={s.Set ? "ok" : "warn"} />
            <div className="min-w-0 flex-1">
              <div className="truncate font-mono text-sm">{s.Name}</div>
              <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[11px] text-muted">
                {s.SetLabel}
                <SourceBadge source={s.Source} label={s.SourceLabel} />
                {s.Last4 ? <span className="font-mono">••••{s.Last4}</span> : s.Set && <span>last 4 hidden</span>}
              </div>
            </div>
            <span className="hidden text-xs text-muted sm:inline">{s.Set ? "Replace" : "Set"}</span>
            <ChevronDown className={cn("size-4 shrink-0 text-muted", foldOpen)} />
          </>
        }
      >
        <form
          className="space-y-3"
          autoComplete="off"
          onSubmit={(e) => {
            e.preventDefault();
            if (!value) return;
            // The value is sent once and never kept, even after a refusal.
            void actions.run(s.SetAction, { value });
            setValue("");
          }}
        >
          <Meta items={[["Updated", s.Updated && <time dateTime={s.UpdatedISO}>{s.Updated}</time>]]} />
          <TextField label={`${s.Set ? "Replace" : "Set"} ${s.Name}`} type="password" value={value} onChange={setValue} autoComplete="new-password" mono placeholder="Paste the new value" />
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" type="submit" isDisabled={!value} isPending={actions.pending === s.SetAction}>
              {s.Set ? "Replace value" : "Set value"}
            </Button>
            {s.Stored && (
              <Button size="sm" variant="danger-soft" isPending={actions.pending === s.ClearAction} onPress={() => void actions.run(s.ClearAction)}>
                Clear stored value
              </Button>
            )}
          </div>
          <p className="text-[11px] text-subtle">The value is written once and never shown again.</p>
        </form>
      </Fold>
    </li>
  );
}
