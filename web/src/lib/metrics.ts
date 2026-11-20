import { api } from "./api";

export type Sample = {
  ts: number;
  cpu: number;
  memUsed: number;
  memLimit: number;
  diskUsed?: number;
  diskTotal?: number;
  netRx: number;
  netTx: number;
  blkRead: number;
  blkWrite: number;
};

export type Snapshot = { ts: number; host?: Sample; apps?: Record<string, Sample> };

export type MetricsStatus = {
  containerSource: string;
  cores: number;
  intervalMs: number;
  retentionDays: number;
};

export type Range = "live" | "1h" | "6h" | "24h" | "7d";

export async function loadSeries(scope: string, range: Range): Promise<Sample[]> {
  if (range === "live") {
    const r = await api<{ samples: Sample[] }>(`/metrics/recent?scope=${encodeURIComponent(scope)}`);
    return r.samples;
  }
  const r = await api<{ samples: Sample[] }>(`/metrics/history?scope=${encodeURIComponent(scope)}&range=${range}`);
  return r.samples;
}

// One shared EventSource for all subscribers on the page.
type Listener = (s: Snapshot) => void;
const listeners = new Set<Listener>();
let es: EventSource | null = null;

export function subscribe(fn: Listener): () => void {
  listeners.add(fn);
  if (!es) {
    es = new EventSource("/api/metrics/stream");
    es.onmessage = (ev) => {
      const snap = JSON.parse(ev.data) as Snapshot;
      for (const l of listeners) l(snap);
    };
  }
  return () => {
    listeners.delete(fn);
    if (listeners.size === 0 && es) {
      es.close();
      es = null;
    }
  };
}

/** Append a live sample, keeping only the last `keepMs` of data. */
export function appendSample(list: Sample[], s: Sample, keepMs = 30 * 60 * 1000): Sample[] {
  const cutoff = s.ts - keepMs;
  const out = list.length && list[0].ts < cutoff ? list.filter((x) => x.ts >= cutoff) : list.slice();
  if (!out.length || out[out.length - 1].ts < s.ts) out.push(s);
  return out;
}
