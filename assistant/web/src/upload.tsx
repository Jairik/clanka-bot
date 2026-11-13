// Upload with the feedback a tailnet round trip needs: drag or pick, an honest
// percentage, cancel, and a retry that reuses the chosen file instead of
// making the user find it again.
import { useRef, useState } from "react";
import { Button } from "@heroui/react";
import { Upload, X } from "lucide-react";
import { cn } from "./ui";

const UNITS: Record<string, number> = { B: 1, KB: 1024, MB: 1024 ** 2, GB: 1024 ** 3 };

/** "10 MB" (the view's own wording) as bytes; an unparseable limit is 0 = no limit. */
export function parseSize(limit: string): number {
  const m = /^([\d.]+)\s*(B|KB|MB|GB)?$/i.exec(limit.trim());
  if (!m) return 0;
  return Math.round(parseFloat(m[1]) * (UNITS[(m[2] ?? "B").toUpperCase()] ?? 1));
}

/** Empty when the file is acceptable; otherwise a message that names the cause. */
export function validateUpload(file: File, accept: string, maxBytes: number): string {
  const tokens = accept.split(",").map((t) => t.trim().toLowerCase()).filter(Boolean);
  const name = file.name.toLowerCase();
  const type = (file.type || "").toLowerCase();
  const typeOk = tokens.length === 0 || tokens.some((t) => (t.startsWith(".") ? name.endsWith(t) : type === t));
  if (!typeOk) return `Choose ${accept.split(",").join(" or ")}.`;
  if (maxBytes > 0 && file.size > maxBytes) return `That file is larger than the ${limitOf(maxBytes)} limit.`;
  return "";
}

// Only used for the over-limit message; the caller keeps the view's wording.
const limitOf = (bytes: number) => {
  if (bytes >= UNITS.GB) return `${Math.round((bytes / UNITS.GB) * 10) / 10} GB`;
  if (bytes >= UNITS.MB) return `${Math.round((bytes / UNITS.MB) * 10) / 10} MB`;
  return `${Math.round(bytes / UNITS.KB)} KB`;
};

type Phase = "waiting" | "uploading" | "failed" | "done";

export function UploadRow({
  action, maxUpload, accept, label, extra, onView,
}: {
  action: string;
  maxUpload: string;
  accept: string;
  label: string;
  extra?: (fd: FormData) => void;
  onView: (view: unknown) => void;
}) {
  const input = useRef<HTMLInputElement>(null);
  const xhr = useRef<XMLHttpRequest | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [phase, setPhase] = useState<Phase>("waiting");
  const [progress, setProgress] = useState(0);
  const [error, setError] = useState("");
  const [drag, setDrag] = useState(false);

  const choose = (next: File | undefined) => {
    if (!next) return;
    const bad = validateUpload(next, accept, parseSize(maxUpload));
    setProgress(0);
    if (bad) {
      setFile(null);
      setPhase("failed");
      setError(bad);
      return;
    }
    setFile(next);
    setPhase("waiting");
    setError("");
  };

  const send = (target: File) => {
    const fd = new FormData();
    fd.set("file", target);
    extra?.(fd);
    const req = new XMLHttpRequest();
    xhr.current = req;
    req.open("POST", action);
    req.setRequestHeader("Accept", "application/json");
    req.upload.onprogress = (e) => {
      if (e.lengthComputable) setProgress(Math.round((e.loaded / e.total) * 100));
    };
    req.onload = () => {
      xhr.current = null;
      let view: unknown = null;
      try {
        view = JSON.parse(req.responseText);
      } catch {
        // A refusal may be plain text; that is still the message to show.
      }
      if (req.status >= 200 && req.status < 300) {
        setPhase("done");
        setProgress(100);
        if (view) onView(view);
        return;
      }
      setPhase("failed");
      setError((view as { Error?: string } | null)?.Error || req.responseText || `Upload failed (${req.status}).`);
    };
    req.onerror = () => {
      xhr.current = null;
      setPhase("failed");
      setError("Couldn't reach the assistant.");
    };
    req.onabort = () => {
      xhr.current = null;
      setPhase("waiting");
      setProgress(0);
    };
    setPhase("uploading");
    setProgress(0);
    setError("");
    req.send(fd);
  };

  return (
    <div
      className={cn("border-t px-3.5 py-3", drag && "bg-surface-secondary")}
      onDragOver={(e) => {
        e.preventDefault();
        setDrag(true);
      }}
      onDragLeave={() => setDrag(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDrag(false);
        choose(e.dataTransfer.files?.[0]);
      }}
    >
      <input
        ref={input}
        type="file"
        accept={accept}
        className="hidden"
        onChange={(e) => {
          choose(e.target.files?.[0]);
          e.target.value = "";
        }}
      />
      <div className="flex flex-wrap items-center gap-3">
        <Button size="sm" variant="secondary" isPending={phase === "uploading"} onPress={() => input.current?.click()} isDisabled={phase === "uploading"}>
          <Upload className="size-3.5" /> {label}
        </Button>
        <span className="text-[11px] text-subtle">Up to {maxUpload}. Drag a file here or pick one.</span>
        {file && phase !== "done" && (
          <div className="flex w-full min-w-0 items-center gap-2 text-xs">
            <span className="min-w-0 flex-1 truncate font-mono">{file.name}</span>
            {phase === "uploading" ? (
              <>
                <span
                  role="progressbar"
                  aria-label="Upload progress"
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-valuenow={progress}
                  className="h-1.5 w-24 overflow-hidden bg-default"
                >
                  <span className="block h-full bg-foreground" style={{ width: `${progress}%` }} />
                </span>
                <span className="w-9 text-right tabular-nums text-muted">{progress}%</span>
                <Button isIconOnly size="sm" variant="ghost" aria-label="Cancel upload" onPress={() => xhr.current?.abort()}>
                  <X className="size-4" />
                </Button>
              </>
            ) : (
              <>
                <Button size="sm" variant={phase === "failed" ? "secondary" : "primary"} onPress={() => send(file)}>
                  {phase === "failed" ? "Retry" : "Upload"}
                </Button>
                <Button size="sm" variant="ghost" onPress={() => { setFile(null); setPhase("waiting"); setError(""); }}>
                  Dismiss
                </Button>
              </>
            )}
          </div>
        )}
        {phase === "done" && <span role="status" className="text-xs text-muted">Uploaded.</span>}
      </div>
      {error && (
        <p role="alert" className="mt-1.5 text-xs text-danger">
          {error}
        </p>
      )}
    </div>
  );
}
