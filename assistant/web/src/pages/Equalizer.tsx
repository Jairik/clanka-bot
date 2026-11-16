// One settings section edits all ten bands and preserves Custom across preset changes.
import { Button, Label, Slider } from "@heroui/react";
import { usePageData } from "../api";
import { useActions } from "../forms";
import { Notice, Section } from "../ui";
import type { EqualizerProfile, EqualizerView } from "../types";

const frequencies = ["31 Hz", "63 Hz", "125 Hz", "250 Hz", "500 Hz", "1 kHz", "2 kHz", "4 kHz", "8 kHz", "16 kHz"];
export function Equalizer() {
  const { data: view, error, set } = usePageData("equalizer");
  const actions = useActions<EqualizerView>(set);
  const busy = !!actions.pending;

  function save(profile: EqualizerProfile) {
    void actions.run("/api/ui/equalizer", { profile: JSON.stringify(profile) });
  }
  return <Section title="Equalizer" summary={view?.Profile.Preset}>
    <p className="mb-4 text-sm text-muted">Shape music, speech and alerts together. Changes apply live and are saved automatically. Health reports when flat audio is used as a fallback.</p>
    <Notice error={error || actions.error || view?.Error} />
    {view && <>
      <div className="mb-5 flex flex-wrap gap-2">{["Flat", "Bass Boost", "Vocal Clarity", "Treble Boost", "Custom"].map(preset =>
        <Button key={preset} size="sm" variant={view.Profile.Preset === preset ? "primary" : "secondary"} isDisabled={busy} onPress={() => void save(preset === "Custom" ? view.Custom : { Preset: preset, Bands: Array(10).fill(0) })}>{preset}</Button>)}</div>
      <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">{frequencies.map((frequency, i) =>
        <Slider key={frequency} minValue={-12} maxValue={12} step={1} value={view.Profile.Bands[i]} isDisabled={busy}
          onChange={value => set({ ...view, Profile: { Preset: "Custom", Bands: view.Profile.Bands.map((gain, band) => band === i ? Number(value) : gain) } })}
          onChangeEnd={value => void save({ Preset: "Custom", Bands: view.Profile.Bands.map((gain, band) => band === i ? Number(value) : gain) })}>
          <Label>{frequency}</Label><Slider.Output>{({ defaultChildren }) => <>{defaultChildren} dB</>}</Slider.Output>
          <Slider.Track><Slider.Fill /><Slider.Thumb /></Slider.Track>
        </Slider>)}</div>
    </>}
  </Section>;
}
