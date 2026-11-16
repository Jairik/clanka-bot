// Monster Smart Lighting over the Ayla cloud: one card per light, with power,
// brightness, white and colour. Scenes are the effect slots the Monster app
// exposes. Every write is one cloud call, so sliders commit when they settle.
import { useEffect, useRef, useState } from "react";
import { Button } from "@heroui/react";
import { usePageData } from "../api";
import { SelectField, useActions } from "../forms";
import { PageFrame } from "../shell";
import { Empty, Lede, Notice, Range, Section } from "../ui";
import type { MonsterDevice, MonsterView } from "../types";

// Writes are JSON, not the form encoding the other pages use.
const JSON_INIT: RequestInit = { headers: { "Content-Type": "application/json", Accept: "application/json" } };

// The white slider is a 0-100 control, while the cloud takes Kelvin. Both round
// to 50 K, the step the Monster app exposes.
const KELVIN_MIN = 2700;
const KELVIN_MAX = 6500;

const clampKelvin = (kelvin: number) => Math.min(KELVIN_MAX, Math.max(KELVIN_MIN, kelvin));
/** Kelvin for a 0-100 slider position, rounded to the nearest 50 K. */
const kelvinAt = (percent: number) => Math.round(clampKelvin(KELVIN_MIN + (percent * (KELVIN_MAX - KELVIN_MIN)) / 100) / 50) * 50;
/** The slider position that shows this device's Kelvin. */
const percentForKelvin = (kelvin: number) => Math.round(((clampKelvin(kelvin) - KELVIN_MIN) / (KELVIN_MAX - KELVIN_MIN)) * 100);

// MonsterPage is the cloud controller page: read the account view, then command
// each light. The cloud is the only source of truth, so every action reloads.
export function MonsterPage() {
  const { data: view, error, reload } = usePageData("monster");
  // The JSON write answers {ok:true}, not a view, so the action has no refreshed
  // view to swap in; the reload below is the single source of truth.
  const actions = useActions<MonsterView>(() => {});
  const busy = !!actions.pending;

  // act runs one cloud action, then refreshes the view. A refused write may
  // still have changed what the account reports, so the reload happens either
  // way and the action's own error stays on the page.
  const act = async (action: string, device: string, value: string) => {
    await actions.run("/api/ui/monster", JSON.stringify({ action, device, value }), JSON_INIT);
    await reload();
  };

  const devices = view?.devices ?? [];

  return (
    <PageFrame id="monster">
      <Notice error={error || actions.error || view?.error} />
      {!view ? (
        error ? null : <Empty>Loading…</Empty>
      ) : view.configured ? (
        <div className="space-y-2.5">
          {view.status && !view.error && (
            <p role="status" className="text-xs text-muted">
              {view.status}
            </p>
          )}
          {devices.length ? (
            devices.map((device) => <MonsterCard key={device.dsn} device={device} busy={busy} onAct={act} />)
          ) : (
            !view.error && <Empty>{view.status || "No Monster lights on this account."}</Empty>
          )}
        </div>
      ) : (
        <Section title="Monster Smart Lighting" summary="Not set up" defaultOpen flush>
          <Lede>
            Monster's Wi-Fi lights, including the Duo+ monitor bar and the strips, are controlled through the Monster
            Smart Lighting account. Add <code className="font-mono">MONSTER_EMAIL</code> and{" "}
            <code className="font-mono">MONSTER_PASSWORD</code> on the{" "}
            <a href="/secrets" className="underline">
              Secrets
            </a>{" "}
            page, then reload this page.
          </Lede>
          {view.status && (
            <p role="status" className="px-3.5 py-2 text-xs text-muted">
              {view.status}
            </p>
          )}
        </Section>
      )}
    </PageFrame>
  );
}

// MonsterCard is one light: its identity in the header, its controls below.
function MonsterCard({
  device,
  busy,
  onAct,
}: {
  device: MonsterDevice;
  busy: boolean;
  onAct: (action: string, device: string, value: string) => Promise<void>;
}) {
  const [scene, setScene] = useState("");
  const effects = device.effects ?? [];
  const scenes = [
    { value: "", label: "Choose a scene" },
    ...effects.map((effect) => ({ value: effect.id, label: effect.name })),
  ];
  return (
    <Section
      title={device.name}
      summary={
        <>
          <span className="font-mono">{device.model}</span>
          {device.mode && <> · {device.mode}</>}
        </>
      }
      tone={device.on ? "ok" : "neutral"}
      defaultOpen
      flush
    >
      <div className="space-y-4 px-3.5 py-3">
        <div className="flex flex-wrap gap-2">
          {(["on", "off"] as const).map((value) => (
            <Button
              key={value}
              size="sm"
              variant="secondary"
              isDisabled={busy}
              aria-label={`${device.name} ${value}`}
              onPress={() => void onAct("power", device.dsn, value)}
            >
              {value === "on" ? "On" : "Off"}
            </Button>
          ))}
        </div>
        <div className="flex flex-col gap-4 sm:flex-row sm:flex-wrap sm:items-end sm:gap-5">
          <CloudRange
            title="Brightness"
            name={device.name}
            initial={device.brightness}
            disabled={busy}
            onCommit={(value) => void onAct("brightness", device.dsn, String(value))}
          />
          <CloudRange
            title="White"
            name={device.name}
            initial={percentForKelvin(device.kelvin)}
            disabled={busy}
            format={(value) => `${kelvinAt(value)} K`}
            onCommit={(value) => void onAct("white", device.dsn, String(kelvinAt(value)))}
          />
          <label className="flex items-center justify-between gap-3 text-xs text-muted sm:w-auto sm:justify-start sm:gap-2">
            Colour
            <input
              type="color"
              aria-label={`${device.name} colour`}
              defaultValue={device.color}
              disabled={busy}
              className="h-8 w-10 cursor-pointer border border-border bg-transparent disabled:cursor-not-allowed disabled:opacity-50"
              onChange={(event) => void onAct("color", device.dsn, event.target.value)}
            />
          </label>
        </div>
        {effects.length > 0 && (
          <SelectField
            label="Scene"
            value={scene}
            options={scenes}
            isDisabled={busy}
            onChange={(value) => {
              setScene("");
              if (value) void onAct("effect", device.dsn, value);
            }}
          />
        )}
        {device.error && (
          <p role="alert" className="text-xs text-danger">
            {device.error}
          </p>
        )}
        <p className="text-[11px] text-subtle">State comes from the Monster cloud.</p>
      </div>
    </Section>
  );
}

// CloudRange commits 400ms after the thumb settles: each commit is one cloud
// write, so a drag must not send a request per step.
function CloudRange({
  title,
  name,
  initial,
  disabled,
  format = String,
  onCommit,
}: {
  title: string;
  name: string;
  initial: number;
  disabled: boolean;
  format?: (value: number) => string;
  onCommit: (value: number) => void;
}) {
  const [value, setValue] = useState(initial);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  // A reload can change the cloud's reported value (another controller, a
  // clamped write); show it unless the user is mid-drag.
  useEffect(() => {
    if (timer.current === undefined) setValue(initial);
  }, [initial]);
  const change = (next: number) => {
    if (disabled) return;
    setValue(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      timer.current = undefined;
      onCommit(next);
    }, 400);
  };
  return (
    <div className="w-full sm:min-w-40 sm:flex-1">
      <div className="mb-1 flex justify-between text-xs text-muted">
        <span>{title}</span>
        <span className="tabular-nums">{format(value)}</span>
      </div>
      <Range label={`${name} ${title.toLowerCase()}`} value={value} onChange={change} isDisabled={disabled} />
    </div>
  );
}
