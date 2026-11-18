// The Lights page is one page for both lighting transports: the Bluetooth room
// controllers the Pi drives itself, and the PC / desk Monster lights that live
// in the cloud. Each transport reads its own view, so a slow or failed cloud
// read never hides or blocks the Bluetooth controls, and the other way round.
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Button, Switch } from "@heroui/react";
import { usePageData } from "../api";
import { SelectField, useActions } from "../forms";
import { PageFrame } from "../shell";
import { Empty, InitialRead, Lede, Notice, Range, Section } from "../ui";
import type { LightsDevice, LightsView, MonsterDevice, MonsterView } from "../types";

// Writes are JSON, not the form encoding the other pages use.
const JSON_INIT: RequestInit = { headers: { "Content-Type": "application/json", Accept: "application/json" } };

const PROTOCOLS = [
  { value: "lotus", label: "Lotus Lantern" },
  { value: "happy", label: "HappyLighting" },
];

const MODES = [
  { value: "rainbow", label: "Rainbow" },
  { value: "pulse", label: "Pulse" },
  { value: "strobe", label: "Strobe" },
];

const protocolLabel = (protocol: string) => PROTOCOLS.find((item) => item.value === protocol)?.label ?? protocol;

// The white slider is a 0-100 control, while the cloud takes Kelvin. Both round
// to 50 K, the step the Monster app exposes.
const KELVIN_MIN = 2700;
const KELVIN_MAX = 6500;

const clampKelvin = (kelvin: number) => Math.min(KELVIN_MAX, Math.max(KELVIN_MIN, kelvin));
/** Kelvin for a 0-100 slider position, rounded to the nearest 50 K. */
const kelvinAt = (percent: number) =>
  Math.round(clampKelvin(KELVIN_MIN + (percent * (KELVIN_MAX - KELVIN_MIN)) / 100) / 50) * 50;
/** The slider position that shows this device's Kelvin. */
const percentForKelvin = (kelvin: number) => Math.round(((clampKelvin(kelvin) - KELVIN_MIN) / (KELVIN_MAX - KELVIN_MIN)) * 100);

// Both transports sit in normal page flow, one after the other: a reader
// scrolls from the room lights to the desk lights, and either can load while
// the other is still waiting.
export function LightsPage() {
  return (
    <PageFrame id="lights">
      <div className="space-y-8">
        <BluetoothLights />
        <MonsterLights />
      </div>
    </PageFrame>
  );
}

/** One transport: its own heading, then its own independent body. */
function Transport({ id, title, hint, children }: { id: string; title: string; hint: ReactNode; children: ReactNode }) {
  return (
    <section aria-labelledby={`${id}-heading`} className="space-y-2.5">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
        <h2 id={`${id}-heading`} className="font-serif text-xl tracking-tight">
          {title}
        </h2>
        <p className="text-xs text-muted">{hint}</p>
      </div>
      {children}
    </section>
  );
}

// BluetoothLights is the room half: opt in, scan, save, then command each saved
// controller. Physical state stays unverified.
function BluetoothLights() {
  const { data: view, error, refreshing, reload } = usePageData("lights");
  const [found, setFound] = useState<LightsDevice[]>([]);
  const [protocol, setProtocol] = useState("lotus");
  const actions = useActions<LightsView>((written) => {
    // A scan answers with the nearby-device list instead of the page view;
    // every other JSON write answers {ok:true}.
    if (Array.isArray(written)) setFound(written as unknown as LightsDevice[]);
  });
  const busy = !!actions.pending;

  // act runs one lights action, then refreshes the saved view. A scan also
  // keeps the nearby list so the user can save a controller.
  const act = async (action: string, extra: object = {}) => {
    await actions.run("/api/ui/lights", JSON.stringify({ action, ...extra }), JSON_INIT);
    await reload();
  };

  const enabled = view?.enabled ?? false;
  const devices = view?.devices ?? [];
  const status = !view ? "" : enabled ? view.status || "Ready to scan" : "Disabled";

  return (
    <Transport id="room-lights" title="Room lights" hint="Bluetooth controllers, straight from the Pi.">
      {!view ? (
        // The first read owns its own failure: no second alert further down.
        <InitialRead error={refreshing ? "" : error} retry={reload} />
      ) : (
        <>
          <Notice error={error || actions.error} />
          <Section title="Bluetooth" summary={enabled ? "On" : "Off"} defaultOpen flush>
            <Lede>
              Experimental. Enable lights before scanning. A successful command does not prove the strip changed. HappyLighting
              brightness sets grayscale RGB.
            </Lede>
            <div className="space-y-2 px-3.5 py-3">
              <Switch isSelected={enabled} isDisabled={busy} onChange={(on) => void act("enable", { value: String(on) })}>
                <Switch.Content className="flex w-full items-center justify-between gap-3">
                  <span className="text-sm">Enable lights</span>
                  <Switch.Control>
                    <Switch.Thumb />
                  </Switch.Control>
                </Switch.Content>
              </Switch>
              <p role="status" className="text-xs text-muted">
                {status}
              </p>
            </div>
            <div className="flex flex-wrap items-end gap-3 px-3.5 pb-3">
              <SelectField
                label="Controller protocol"
                value={protocol}
                options={PROTOCOLS}
                isDisabled={busy}
                onChange={setProtocol}
                className="sm:w-64"
              />
              <Button variant="secondary" isDisabled={busy || !enabled} onPress={() => void act("scan")}>
                Scan for lights
              </Button>
            </div>
          </Section>

          {found.length > 0 && (
            <Section title="Nearby" summary={`${found.length}`} defaultOpen flush>
              <ul className="divide-y divide-separator">
                {found.map((device) => {
                  const saved = devices.some((item) => item.address === device.address);
                  return (
                    <li key={device.address} className="flex items-center justify-between gap-3 px-3.5 py-3">
                      <div className="min-w-0">
                        <p className="truncate text-sm">{device.name}</p>
                        <p className="truncate font-mono text-[11px] text-muted">{device.address}</p>
                      </div>
                      <Button
                        size="sm"
                        variant="secondary"
                        isDisabled={busy || saved}
                        onPress={() => void act("save", { devices: [...devices, { ...device, protocol }] })}
                      >
                        {saved ? "Saved" : "Save light"}
                      </Button>
                    </li>
                  );
                })}
              </ul>
            </Section>
          )}

          <Section title="Saved" summary={`${devices.length}`} defaultOpen flush>
            {devices.length ? (
              <ul className="divide-y divide-separator">
                {devices.map((device) => (
                  <SavedLight
                    key={device.address}
                    device={device}
                    busy={busy}
                    enabled={enabled}
                    onAct={act}
                    onForget={() => void act("save", { devices: devices.filter((item) => item.address !== device.address) })}
                  />
                ))}
              </ul>
            ) : (
              <Empty>No lights saved. Scan, pick a protocol, and save a controller.</Empty>
            )}
          </Section>
        </>
      )}
    </Transport>
  );
}

// MonsterLights is the PC / desk half: read the account view, then command each
// light. The cloud is the only source of truth, so every action reloads.
function MonsterLights() {
  const { data: view, error, refreshing, reload } = usePageData("monster");
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
    <Transport id="desk-lights" title="PC / desk lights" hint="The Monster account, over the cloud.">
      {!view ? (
        <InitialRead error={refreshing ? "" : error} retry={reload} />
      ) : view.configured ? (
        <>
          <Notice error={error || actions.error || view.error} />
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
        </>
      ) : (
        <Section title="Monster Smart Lighting" summary="Not set up" defaultOpen flush>
          <Lede>
            Monster's Wi-Fi lights, including the Duo+ monitor bar and the strips, are controlled through the Monster Smart
            Lighting account. Add <code className="font-mono">MONSTER_EMAIL</code> and{" "}
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
    </Transport>
  );
}

// SavedLight is one remembered Bluetooth controller: power, brightness, colour
// and mode.
function SavedLight({
  device,
  busy,
  enabled,
  onAct,
  onForget,
}: {
  device: LightsDevice;
  busy: boolean;
  enabled: boolean;
  onAct: (action: string, extra?: object) => Promise<void>;
  onForget: () => void;
}) {
  const locked = busy || !enabled;
  return (
    <li className="space-y-4 px-3.5 py-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <p className="truncate text-sm">{device.name}</p>
          <p className="truncate font-mono text-[11px] text-muted">
            {protocolLabel(device.protocol)} · {device.address}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          {["on", "off"].map((value) => (
            <Button key={value} size="sm" variant="secondary" isDisabled={locked} aria-label={`${device.name} ${value}`} onPress={() => void onAct("power", { device: device.address, value })}>
              {value === "on" ? "On" : "Off"}
            </Button>
          ))}
          <Button size="sm" variant="ghost" isDisabled={busy} aria-label={`Forget ${device.name}`} onPress={onForget}>
            Forget
          </Button>
        </div>
      </div>
      <div className="flex flex-wrap items-end gap-5">
        <Brightness name={device.name} disabled={locked} onCommit={(value) => void onAct("brightness", { device: device.address, value })} />
        <label className="flex items-center gap-2 text-xs text-muted">
          Colour
          <input
            type="color"
            aria-label={`${device.name} colour`}
            defaultValue="#ffffff"
            disabled={locked}
            className="h-8 w-10 cursor-pointer border border-border bg-transparent disabled:cursor-not-allowed disabled:opacity-50"
            onChange={(event) => void onAct("color", { device: device.address, value: event.target.value })}
          />
        </label>
        <div className="flex flex-wrap gap-2">
          {MODES.map((mode) => (
            <Button
              key={mode.value}
              size="sm"
              variant="secondary"
              isDisabled={locked}
              aria-label={`${device.name} ${mode.label}`}
              onPress={() => void onAct("mode", { device: device.address, value: mode.value })}
            >
              {mode.label}
            </Button>
          ))}
        </div>
      </div>
    </li>
  );
}

// Brightness commits after the thumb settles. Each value opens a Bluetooth
// connection, so a drag must not send a write per step.
function Brightness({ name, disabled, onCommit }: { name: string; disabled: boolean; onCommit: (value: string) => void }) {
  const [value, setValue] = useState(100);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const change = (next: number) => {
    if (disabled) return;
    setValue(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => onCommit(String(next)), 400);
  };
  return (
    <div className="min-w-40 flex-1">
      <div className="mb-1 flex justify-between text-xs text-muted">
        <span>Brightness</span>
        <span className="tabular-nums">{value}</span>
      </div>
      <Range label={`${name} brightness`} value={value} onChange={change} />
    </div>
  );
}

// MonsterCard is one cloud light: its identity in the header, its controls
// below. Scenes are the effect slots the Monster app exposes.
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
  const scenes = [{ value: "", label: "Choose a scene" }, ...effects.map((effect) => ({ value: effect.id, label: effect.name }))];
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
