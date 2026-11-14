import { useEffect, useState } from "react";
import { Button } from "@heroui/react";
import { Section } from "../ui";

type Device = {address: string; name: string; protocol: string};
type View = {enabled: boolean; devices: Device[]; status: string};
// Lights is part of Settings so discovery and the opt-in stay together.
export function LightsSection() {
 const [view,setView]=useState<View>(); const [found,setFound]=useState<Device[]>([]);
 const [error,setError]=useState(""); const [busy,setBusy]=useState(false);
 const [protocol,setProtocol]=useState("lotus");
 const load=async()=>{const r=await fetch("/api/ui/lights");if(!r.ok)throw Error(await r.text());setView(await r.json());};
 useEffect(()=>{load().catch(e=>setError(String(e)));},[]);
 const act=async(action:string,extra:object={})=>{setBusy(true);setError("");try{const r=await fetch("/api/ui/lights",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({action,...extra})});if(!r.ok)throw Error(await r.text());if(action==="scan")setFound(await r.json());await load();}catch(e){setError(String(e));}finally{setBusy(false);}};
 return <Section title="Bluetooth lights" defaultOpen>
  <div className="space-y-4 p-4 text-sm">
   <p>Experimental. Enable lights before scanning. Physical state is unverified. HappyLighting brightness sets grayscale RGB.</p>
   {error && <p role="alert">{error}</p>}
   <p role="status">{view?.enabled ? view.status || "Ready to scan" : "Disabled"}</p>
   <Button variant="secondary" isDisabled={busy || !view} onPress={()=>void act("enable",{value:String(!view?.enabled)})}>{view?.enabled ? "Disable lights" : "Enable lights"}</Button>
   <Button variant="secondary" isDisabled={busy || !view?.enabled} onPress={()=>void act("scan")}>Scan for lights</Button>
   <label className="block">Controller protocol <select value={protocol} onChange={e=>setProtocol(e.target.value)}><option value="lotus">Lotus Lantern / ELK-BLEDOM</option><option value="happy">HappyLighting / Triones</option></select></label>
   {found.map(d=><div key={d.address} className="flex items-center justify-between gap-3"><span>{d.name} · {d.address}</span><Button variant="secondary" isDisabled={busy || view?.devices.some(x=>x.address===d.address)} onPress={()=>void act("save",{devices:[...(view?.devices??[]),{...d,protocol}]})}>Save light</Button></div>)}
   {view?.devices.map(d=><div key={d.address} className="space-y-3 border-t border-separator pt-3">
    <p>{d.name} · {d.protocol} · {d.address}</p>
    <div className="flex flex-wrap gap-3">
     {["on","off"].map(value=><Button key={value} variant="secondary" isDisabled={busy || !view.enabled} onPress={()=>void act("power",{device:d.address,value})}>{value}</Button>)}
     <Button variant="ghost" isDisabled={busy} onPress={()=>void act("save",{devices:view.devices.filter(x=>x.address!==d.address)})}>Forget</Button>
    </div>
    <fieldset disabled={busy || !view.enabled} className="flex flex-wrap gap-5">
     <label>Brightness <input aria-label={`${d.name} brightness`} type="range" min="0" max="100" defaultValue="100" onPointerUp={e=>void act("brightness",{device:d.address,value:e.currentTarget.value})} onKeyUp={e=>{if(["ArrowLeft","ArrowRight","Home","End"].includes(e.key))void act("brightness",{device:d.address,value:e.currentTarget.value});}} /></label>
     <label>Colour <input type="color" defaultValue="#ffffff" onChange={e=>void act("color",{device:d.address,value:e.target.value})}/></label>
     <label>Mode <select defaultValue="" onChange={e=>void act("mode",{device:d.address,value:e.target.value})}><option value="" disabled>Choose mode</option>{["rainbow","pulse","strobe"].map(m=><option key={m}>{m}</option>)}</select></label>
    </fieldset>
   </div>)}
  </div>
 </Section>;
}
