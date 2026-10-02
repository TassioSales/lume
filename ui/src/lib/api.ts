export type TableMeta = { num: number; level: number; size: number; entries: number; tombstones: number; min: string; max: string };

export type Event = { at: string; kind: "flush" | "compaction" | "recovery"; detail: string; inputs?: TableMeta[]; outputs?: TableMeta[]; dropped: number; millis: number };

export type Stats = {
  puts: number;
  deletes: number;
  gets: number;
  hits: number;
  bloomSkips: number;
  blockReads: number;
  wastedReads: number;
  flushes: number;
  compactions: number;
  userBytes: number;
  tableBytes: number;
  walBytes: number;
};

export type View = {
  memtable: { entries: number; bytes: number; limit: number };
  flushing: boolean;
  levels: TableMeta[][];
  limits: number[];
  stats: Stats;
  events: Event[];
};

export type Step = { where: string; level: number; table?: number; result: string };

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error ?? `HTTP ${res.status}`);
  return data as T;
}

export const api = {
  view: () => call<{ view: View; sync: boolean; nextId: number }>("GET", "/view"),
  get: (key: string) => call<{ found: boolean; value: string; steps: Step[]; micros: number }>("GET", `/get?key=${encodeURIComponent(key)}`),
  scan: (prefix: string) => call<{ pairs: { key: string; value: string }[] }>("GET", `/scan?prefix=${encodeURIComponent(prefix)}`).then((r) => r.pairs),
  put: (key: string, value: string) => call<void>("POST", "/put", { key, value }),
  del: (key: string) => call<void>("POST", "/delete", { key }),
  load: (kind: "new" | "overwrite" | "delete", n: number) => call<{ millis: number }>("POST", "/load", { kind, n }),
  flush: () => call<void>("POST", "/flush"),
  crash: () => call<{ inMemory: number; millis: number }>("POST", "/crash"),
  reset: () => call<void>("POST", "/reset"),
};

export const count = (n: number) => n.toLocaleString("pt-BR");

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0).replace(".", ",")} KB`;
  return `${(n / (1 << 20)).toFixed(1).replace(".", ",")} MB`;
}

/** The colour of a level: the memory table is the flame, deep levels are cold. */
export const heat = ["var(--color-hot-1)", "var(--color-hot-2)", "var(--color-hot-3)", "var(--color-hot-4)", "var(--color-hot-5)", "var(--color-hot-6)"];
