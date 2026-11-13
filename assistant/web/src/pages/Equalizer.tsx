// One settings section edits all ten bands and preserves Custom across preset changes.
import { useEffect, useState } from "react";
import { Button, Label, Slider } from "@heroui/react";
import { getJSON, postForm } from "../api";
import { Notice, Section } from "../ui";

type Profile = { Preset: string; Bands: number[] };
type View = { Profile: Profile; Custom: Profile; Error: string };
const frequencies = ["31 Hz", "63 Hz", "125 Hz", "250 Hz", "500 Hz", "1 kHz", "2 kHz", "4 kHz", "8 kHz", "16 kHz"];
export function Equalizer() {
  const [view, setView] = useState<View | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => { const abort = new AbortController(); getJSON<View>("/api/ui/equalizer", abort.signal).then(setView).catch(e => { if (!abort.signal.aborted) setError(String(e)); }); return () => abort.abort(); }, []);
  async function save(profile: Profile) {
    setBusy(true); setError("");
    try { const result = await postForm<View>("/api/ui/equalizer", { profile: JSON.stringify(profile) });
      if (result.view) { setView(result.view); setError(result.view.Error); }
      if (!result.ok) setError(result.view?.Error || result.text || "Could not save equalizer.");
    } catch (e) { setError(String(e)); } finally { setBusy(false); }
  }
  return <Section title="Equalizer" summary={view?.Profile.Preset}>
    <p className="mb-4 text-sm text-muted">Shape music, speech and alerts together. Changes apply live and are saved automatically. Health reports when flat audio is used as a fallback.</p>
    <Notice error={error || view?.Error} />
    {view && <>
      <div className="mb-5 flex flex-wrap gap-2">{["Flat", "Bass Boost", "Vocal Clarity", "Treble Boost", "Custom"].map(preset =>
        <Button key={preset} size="sm" variant={view.Profile.Preset === preset ? "primary" : "secondary"} isDisabled={busy} onPress={() => void save(preset === "Custom" ? view.Custom : { Preset: preset, Bands: Array(10).fill(0) })}>{preset}</Button>)}</div>
      <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">{frequencies.map((frequency, i) =>
        <Slider key={frequency} minValue={-12} maxValue={12} step={1} value={view.Profile.Bands[i]} isDisabled={busy}
          onChange={value => setView({ ...view, Profile: { Preset: "Custom", Bands: view.Profile.Bands.map((gain, band) => band === i ? Number(value) : gain) } })}
          onChangeEnd={value => void save({ Preset: "Custom", Bands: view.Profile.Bands.map((gain, band) => band === i ? Number(value) : gain) })}>
          <Label>{frequency}</Label><Slider.Output>{({ defaultChildren }) => <>{defaultChildren} dB</>}</Slider.Output>
          <Slider.Track><Slider.Fill /><Slider.Thumb /></Slider.Track>
        </Slider>)}</div>
    </>}
  </Section>;
}
