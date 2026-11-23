// Hermes job provider: which model backend runs the next Hermes job. The
// server owns the offered OpenCode Go model IDs and checks readiness; this
// section only shows the two choices and saves the selection.
import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Radio, RadioGroup } from "@heroui/react";
import { HttpError, getJSON, postForm } from "../api";
import { SelectField } from "../forms";
import { Badge, Empty, InitialRead, Lede, Notice, Section } from "../ui";
import { apiError } from "./HermesTools";

export type HermesProvider = "local" | "opencode-go";
export type HermesRuntimeView = {
  provider: HermesProvider;
  open_code_go_model: string;
  /** Server-owned IDs; the only models a save may name. */
  open_code_go_models: string[] | null;
  local_model: string;
  local_ready: boolean;
  open_code_go_ready: boolean;
};

export const RUNTIME_PATH = "/api/ui/hermes/runtime";
export const DEFAULT_OPEN_CODE_GO_MODEL = "deepseek-v4.1-flash";
const JSON_INIT: RequestInit = { headers: { "Content-Type": "application/json", Accept: "application/json" } };

const isView = (v: unknown): v is HermesRuntimeView =>
  !!v &&
  typeof v === "object" &&
  ((v as HermesRuntimeView).provider === "local" || (v as HermesRuntimeView).provider === "opencode-go") &&
  typeof (v as HermesRuntimeView).open_code_go_model === "string";

/** The model to preselect: the saved one while it is still offered, else the default, else the first offered. */
export function initialModel(view: HermesRuntimeView): string {
  const offered = view.open_code_go_models ?? [];
  if (offered.includes(view.open_code_go_model)) return view.open_code_go_model;
  if (offered.includes(DEFAULT_OPEN_CODE_GO_MODEL)) return DEFAULT_OPEN_CODE_GO_MODEL;
  return offered[0] ?? view.open_code_go_model;
}

/** Loads the runtime view and saves it. Newest write wins. */
function useRuntime() {
  const [data, setData] = useState<HermesRuntimeView | null>(null);
  const [loadError, setLoadError] = useState("");
  const [missing, setMissing] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [flash, setFlash] = useState("");
  const seq = useRef(0);

  const load = useCallback(async () => {
    setLoadError("");
    try {
      const view = await getJSON<unknown>(RUNTIME_PATH);
      if (!isView(view)) throw new Error("The assistant answered without a job provider. Reload to try again.");
      setData(view);
      setMissing(false);
    } catch (e) {
      // An assistant from before this control answers 404 during a mixed
      // release: say so instead of offering a retry that cannot succeed.
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

  const save = async (provider: HermesProvider, model: string) => {
    const mine = ++seq.current;
    setPending(true);
    setError("");
    setFlash("");
    try {
      const res = await postForm<unknown>(RUNTIME_PATH, JSON.stringify({ provider, open_code_go_model: model }), JSON_INIT);
      if (mine !== seq.current) return;
      if (res.ok && isView(res.view)) {
        setData(res.view);
        setFlash(res.view.provider === "local" ? "New jobs run locally." : "New jobs run on OpenCode Go.");
      } else if (res.ok) {
        setError("The assistant answered without a job provider. Reload to see the current state.");
      } else if (res.status === 404) {
        setError("This assistant cannot change the job provider yet.");
      } else {
        setError(apiError(res.view) || apiError(res.text) || res.text || `Request failed (${res.status}).`);
      }
    } catch (e) {
      if (mine === seq.current) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (mine === seq.current) setPending(false);
    }
  };

  return { data, loadError, missing, load, pending, error, flash, save, clear: () => (setError(""), setFlash("")) };
}

const ready = (ok: boolean) => <Badge tone={ok ? "ok" : "warn"}>{ok ? "ready" : "not ready"}</Badge>;

export function HermesRuntimeSection() {
  const rt = useRuntime();
  const { data } = rt;
  const localName = data?.local_model.trim() || "Ornith";

  if (rt.missing) {
    return (
      <Section title="Job provider" defaultOpen>
        <Empty>This assistant does not offer a job provider choice yet. Jobs use its built-in provider.</Empty>
      </Section>
    );
  }

  return (
    <Section
      title="Job provider"
      summary={data ? (data.provider === "local" ? `Local · ${localName}` : "OpenCode Go") : undefined}
      tone={data && !(data.provider === "local" ? data.local_ready : data.open_code_go_ready) ? "warn" : undefined}
      defaultOpen
      flush
    >
      {!data ? (
        <InitialRead className="px-3.5" error={rt.loadError} retry={rt.load} />
      ) : (
        <RuntimeForm view={data} localName={localName} rt={rt} />
      )}
    </Section>
  );
}

function RuntimeForm({ view, localName, rt }: { view: HermesRuntimeView; localName: string; rt: ReturnType<typeof useRuntime> }) {
  const offered = view.open_code_go_models ?? [];
  const [provider, setProvider] = useState<HermesProvider>(view.provider);
  const [model, setModel] = useState(() => initialModel(view));
  // Follow the server's copy after a save (or another tab's).
  const serverKey = JSON.stringify(view);
  useEffect(() => {
    setProvider(view.provider);
    setModel(initialModel(view));
  }, [serverKey]); // view is read through serverKey

  const cloud = provider === "opencode-go";
  const savedGone = view.open_code_go_model !== "" && !offered.includes(view.open_code_go_model);
  const changed = provider !== view.provider || model !== view.open_code_go_model;
  const blocked = cloud && !model;

  return (
    <form
      noValidate
      aria-label="Hermes job provider"
      onSubmit={(e) => {
        e.preventDefault();
        if (!blocked) void rt.save(provider, model);
      }}
    >
      <Lede>
        New Hermes jobs use the provider saved here. Choosing OpenCode Go sends job requests to OpenCode Go. There is no automatic
        fallback: a job never switches to the other provider on its own.
      </Lede>
      <div className="space-y-3 px-3.5 py-3">
        <Notice error={rt.error} flash={rt.flash} onDismiss={rt.clear} />

        <RadioGroup aria-label="Provider for new Hermes jobs" value={provider} onChange={(v) => setProvider(v as HermesProvider)} className="gap-3">
          <Radio value="local">
            <Radio.Control>
              <Radio.Indicator />
            </Radio.Control>
            <Radio.Content>
              <span className="flex flex-wrap items-center gap-1.5 text-sm">
                Local <span className="font-mono text-xs text-muted">{localName}</span> {ready(view.local_ready)}
              </span>
              <span className="mt-0.5 block text-[11px] text-muted">
                {view.local_ready ? "Runs on your own hardware." : "The local model is not reachable right now."}
              </span>
            </Radio.Content>
          </Radio>
          <Radio value="opencode-go">
            <Radio.Control>
              <Radio.Indicator />
            </Radio.Control>
            <Radio.Content>
              <span className="flex flex-wrap items-center gap-1.5 text-sm">
                OpenCode Go {ready(view.open_code_go_ready)}
              </span>
              <span className="mt-0.5 block text-[11px] text-muted">
                {view.open_code_go_ready ? (
                  "A cloud provider: job requests leave this network."
                ) : (
                  <>
                    Add the OpenCode Go key in{" "}
                    <a href="/secrets" className="underline">
                      Secrets
                    </a>{" "}
                    before choosing it.
                  </>
                )}
              </span>
            </Radio.Content>
          </Radio>
        </RadioGroup>

        <div className="space-y-1.5">
          <span className="text-xs text-muted">OpenCode Go model</span>
          {offered.length ? (
            <SelectField
              label="OpenCode Go model"
              value={model}
              onChange={setModel}
              options={offered.map((id) => ({ value: id, label: id }))}
              className="sm:w-72"
            />
          ) : (
            <p className="text-[11px] text-muted">No OpenCode Go model is offered by this assistant.</p>
          )}
          <span className="block text-[11px] text-muted">
            {savedGone
              ? `The saved model ${view.open_code_go_model} is no longer offered; choose another.`
              : "Offered models have documented zero-day request retention and tool-call support. The model is kept while Local is selected."}
          </span>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" type="submit" isPending={rt.pending} isDisabled={!changed || blocked}>
            Save
          </Button>
          {cloud && !view.open_code_go_ready && (
            <span role="status" className="text-[11px] text-muted">
              OpenCode Go is not ready; jobs cannot run on it until its key is set.
            </span>
          )}
        </div>
      </div>
    </form>
  );
}
