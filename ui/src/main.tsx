import clsx from "clsx";
import { ArrowDown, Flame, HardDriveDownload, Layers, RotateCcw, Search, Skull, Trash2, Zap } from "lucide-react";
import { StrictMode, useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { api, bytes, count, heat, type Event, type Step, type TableMeta, type View } from "./lib/api";

function App() {
  const [data, setData] = useState<{ view: View; sync: boolean; nextId: number } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<{ text: string; tone: "ok" | "bad" } | null>(null);
  const refresh = useCallback(() => api.view().then((d) => (setData(d), setError(null)), (e) => setError(String(e.message ?? e))), []);
  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 700);
    return () => clearInterval(t);
  }, [refresh]);

  const run = async (fn: () => Promise<string | void>) => {
    try {
      const text = await fn();
      if (text) setNote({ text, tone: "ok" });
      await refresh();
    } catch (e) {
      setNote({ text: e instanceof Error ? e.message : String(e), tone: "bad" });
    }
  };

  if (!data) return <p className="label p-16 text-center">{error ? `Sem conexão com o servidor: ${error}` : "Abrindo o banco…"}</p>;
  const v = data.view;
  const s = v.stats;
  const tables = v.levels.flat();
  const disk = tables.reduce((n, t) => n + t.size, 0);

  return (
    <div className="mx-auto max-w-7xl px-5 py-6">
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <div className="flex items-center gap-2.5 text-3xl font-extrabold tracking-tight">
            <Flame className="size-8 fill-ember text-flame" /> lume
          </div>
          <p className="mt-1 max-w-2xl text-[15px] leading-relaxed text-ink-2">
            Um banco chave-valor escrito do zero como uma árvore LSM. Nada é alterado no lugar: escritas entram pelo topo, quentes, e vão sendo fundidas em arquivos cada vez maiores e mais frios.
          </p>
        </div>
        <div className="flex gap-2">
          <Ghost onClick={() => run(async () => (await api.flush(), "Memtable gravada em disco."))} icon={<HardDriveDownload className="size-4" />}>
            Gravar memtable
          </Ghost>
          <Ghost onClick={() => window.confirm("Apagar todos os dados da demonstração?") && run(async () => (await api.reset(), "Banco zerado."))} icon={<RotateCcw className="size-4" />}>
            Zerar
          </Ghost>
        </div>
      </header>

      {note && (
        <p className={clsx("rise mt-4 rounded-lg border px-4 py-2.5 text-[13px] font-medium", note.tone === "ok" ? "border-ember/50 bg-ember/10 text-ink" : "border-bad/60 bg-bad/10 text-bad")} role="status">
          {note.text}
        </p>
      )}

      <div className="mt-5 grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        <Tile label="Escritas" value={count(s.puts + s.deletes)} sub={`${count(s.deletes)} são marcas de apagado`} />
        <Tile label="Leituras" value={count(s.gets)} sub={`${count(s.hits)} acharam a chave`} />
        <Tile label="Disco poupado" value={count(s.bloomSkips)} sub={`leituras evitadas pelo filtro de Bloom; ${count(s.wastedReads)} em vão`} glow />
        <Tile label="Amplificação" value={s.userBytes ? `${(s.tableBytes / s.userBytes).toFixed(1).replace(".", ",")}×` : "—"} sub="bytes gravados em tabelas por byte pedido" />
        <Tile label="Em disco" value={bytes(disk)} sub={`${tables.length} ${tables.length === 1 ? "tabela" : "tabelas"} · ${count(tables.reduce((n, t) => n + t.entries, 0))} entradas`} />
        <Tile label="Fundo" value={`${s.flushes} / ${s.compactions}`} sub="gravações de memtable / compactações" />
      </div>

      <div className="mt-6 grid items-start gap-6 xl:grid-cols-[1fr_400px]">
        <div className="space-y-6">
          <Tree view={v} />
          <Events events={v.events} />
        </div>
        <div className="space-y-6">
          <Load nextId={data.nextId} run={run} sync={data.sync} />
          <Read run={run} nextId={data.nextId} />
          <Explorer rev={s.puts + s.deletes + s.compactions} />
        </div>
      </div>
    </div>
  );
}

function Ghost({ icon, children, ...rest }: React.ButtonHTMLAttributes<HTMLButtonElement> & { icon?: ReactNode }) {
  return (
    <button type="button" {...rest} className="inline-flex h-9 items-center gap-2 rounded-lg border border-line bg-ash px-3 text-[13px] font-semibold text-ink-2 transition-colors hover:border-ember hover:text-ink disabled:opacity-40">
      {icon}
      {children}
    </button>
  );
}

function Panel({ title, hint, icon, children }: { title: string; hint?: string; icon?: ReactNode; children: ReactNode }) {
  return (
    <section className="rounded-2xl border border-line bg-ash">
      <header className="border-b border-line px-5 py-3.5">
        <h2 className="flex items-center gap-2 text-base font-bold">
          {icon}
          {title}
        </h2>
        {hint && <p className="mt-1 text-xs leading-relaxed text-ink-3">{hint}</p>}
      </header>
      <div className="p-5">{children}</div>
    </section>
  );
}

function Tile({ label, value, sub, glow }: { label: string; value: string; sub: string; glow?: boolean }) {
  return (
    <div className={clsx("rounded-xl border border-line bg-ash px-4 py-3", glow && "shadow-glow")}>
      <div className="label">{label}</div>
      <div className="mono mt-1 text-2xl leading-none font-bold">{value}</div>
      <div className="mt-1.5 text-[11px] leading-snug text-ink-3">{sub}</div>
    </div>
  );
}

// ---- the tree ---------------------------------------------------------------

/** The store from top to bottom: memory, then each level on disk. */
function Tree({ view }: { view: View }) {
  // Tables first seen in this render glow as they appear.
  const seen = useRef<Set<number> | null>(null);
  const fresh = new Set<number>();
  const all = view.levels.flat().map((t) => t.num);
  if (seen.current) for (const n of all) if (!seen.current.has(n)) fresh.add(n);
  useEffect(() => {
    seen.current = new Set(all);
  });
  const biggest = Math.max(1, ...view.levels.flat().map((t) => t.size));
  const fill = Math.min(1, view.memtable.bytes / view.memtable.limit);
  const deepest = Math.max(2, ...view.levels.map((l, i) => (l.length ? i : 0)));

  return (
    <Panel title="A árvore" icon={<Layers className="size-4 text-ember" />} hint="Cada bloco é um arquivo imutável e ordenado. A largura é o tamanho. Passe o mouse para ver o intervalo de chaves.">
      <div className="flex items-center gap-4">
        <div className="w-24 shrink-0">
          <div className="text-[13px] font-bold text-flame">Memtable</div>
          <div className="text-[11px] text-ink-3">na memória</div>
        </div>
        <div className="min-w-0 flex-1">
          <div className="h-9 overflow-hidden rounded-lg border border-line bg-coal">
            <div className={clsx("h-full rounded-r-md bg-gradient-to-r from-ember to-flame transition-[width] duration-500", view.flushing && "flicker")} style={{ width: `${fill * 100}%` }} />
          </div>
          <div className="mono mt-1 flex justify-between text-[11px] text-ink-3">
            <span>
              {count(view.memtable.entries)} entradas · {bytes(view.memtable.bytes)} de {bytes(view.memtable.limit)}
            </span>
            <span>{view.flushing ? "a anterior está sendo gravada em disco…" : "também no log, para sobreviver a uma queda"}</span>
          </div>
        </div>
      </div>

      {view.levels.slice(0, deepest + 1).map((tables, level) => {
        const used = level === 0 ? tables.length : tables.reduce((n, t) => n + t.size, 0);
        const limit = view.limits[level]!;
        return (
          <div key={level}>
            <div className="my-2 flex justify-center">
              <ArrowDown className="size-4 text-line" />
            </div>
            <div className="flex items-center gap-4">
              <div className="w-24 shrink-0">
                <div className="text-[13px] font-bold" style={{ color: heat[level] }}>
                  Nível {level}
                </div>
                <div className="mono text-[11px] text-ink-3">{level === 0 ? `${used} de ${limit} arq.` : `${bytes(used)} de ${bytes(limit)}`}</div>
              </div>
              <div className="flex min-h-11 min-w-0 flex-1 flex-wrap items-end gap-1 rounded-lg border border-line bg-coal p-1.5">
                {tables.length === 0 && <span className="px-2 py-1.5 text-[11px] text-ink-3">vazio</span>}
                {tables.map((t) => (
                  <TableBlock key={t.num} t={t} share={t.size / biggest} fresh={fresh.has(t.num)} />
                ))}
              </div>
            </div>
            {level === 0 && <p className="mt-1 pl-28 text-[11px] text-ink-3">Arquivos do nível 0 podem se sobrepor: uma leitura consulta todos, do mais novo ao mais antigo.</p>}
            {level === 1 && tables.length > 0 && <p className="mt-1 pl-28 text-[11px] text-ink-3">Do nível 1 para baixo os arquivos não se sobrepõem: no máximo um por nível pode ter a chave.</p>}
          </div>
        );
      })}
    </Panel>
  );
}

function TableBlock({ t, share, fresh }: { t: TableMeta; share: number; fresh: boolean }) {
  return (
    <div
      className={clsx("relative h-8 rounded-md", fresh && "kindle")}
      style={{ width: 26 + Math.round(share * 64), background: heat[t.level], opacity: 0.92 }}
      title={`tabela ${t.num} · ${bytes(t.size)} · ${t.entries} entradas (${t.tombstones} apagadas)\n${t.min}  →  ${t.max}`}
    >
      <span className="mono absolute inset-0 flex items-center justify-center overflow-hidden text-[10px] font-bold text-coal">{t.num}</span>
    </div>
  );
}

// ---- events -----------------------------------------------------------------

const kinds: Record<Event["kind"], { label: string; tone: string }> = {
  flush: { label: "gravação", tone: "text-flame border-flame/40" },
  compaction: { label: "compactação", tone: "text-ember border-ember/50" },
  recovery: { label: "recuperação", tone: "text-ok border-ok/50" },
};

function Chips({ tables }: { tables?: TableMeta[] }) {
  return (
    <span className="inline-flex flex-wrap gap-1">
      {(tables ?? []).map((t) => (
        <span key={t.num} className="mono rounded px-1.5 py-px text-[10px] font-bold text-coal" style={{ background: heat[t.level] }} title={`${bytes(t.size)} · ${t.entries} entradas`}>
          {t.num}
        </span>
      ))}
    </span>
  );
}

function Events({ events }: { events: Event[] }) {
  const list = [...events].reverse().slice(0, 14);
  return (
    <Panel title="O que o banco fez sozinho" hint="Gravar a memtable e fundir arquivos acontece em segundo plano. Compactar é o preço de escritas rápidas: reescreve dados para jogar fora o que foi sobrescrito ou apagado.">
      {list.length === 0 ? (
        <p className="text-[13px] text-ink-3">Nada ainda. Escreva algumas milhares de chaves.</p>
      ) : (
        <ol className="space-y-2.5">
          {list.map((e, i) => {
            const k = kinds[e.kind];
            const inBytes = (e.inputs ?? []).reduce((n, t) => n + t.size, 0);
            const outBytes = (e.outputs ?? []).reduce((n, t) => n + t.size, 0);
            return (
              <li key={`${e.at}-${i}`} className={clsx("flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px]", i === 0 && "rise")}>
                <span className={clsx("w-24 shrink-0 rounded-full border px-2 py-px text-center text-[11px] font-bold", k.tone)}>{k.label}</span>
                {e.kind === "compaction" ? (
                  <>
                    <Chips tables={e.inputs} />
                    <span className="text-ink-3">→</span>
                    <Chips tables={e.outputs} />
                    <span className="text-ink-2">
                      {e.detail} · {bytes(inBytes)} → {bytes(outBytes)}
                      {e.dropped > 0 && <b className="text-ember"> · {count(e.dropped)} entradas descartadas</b>}
                    </span>
                  </>
                ) : (
                  <>
                    <Chips tables={e.outputs} />
                    <span className="text-ink-2">{e.detail}</span>
                  </>
                )}
                <span className="mono ml-auto text-[11px] text-ink-3">{e.millis.toFixed(0)} ms</span>
              </li>
            );
          })}
        </ol>
      )}
    </Panel>
  );
}

// ---- controls ---------------------------------------------------------------

type Run = (fn: () => Promise<string | void>) => Promise<void>;

function Load({ nextId, run, sync }: { nextId: number; run: Run; sync: boolean }) {
  const [busy, setBusy] = useState(false);
  const go = (fn: () => Promise<string | void>) => async () => {
    setBusy(true);
    await run(fn);
    setBusy(false);
  };
  const load = (kind: "new" | "overwrite" | "delete", n: number, what: string) =>
    go(async () => {
      const r = await api.load(kind, n);
      return `${count(n)} ${what} em ${r.millis.toFixed(0)} ms (${count(Math.round(n / (r.millis / 1000)))} por segundo).`;
    });
  return (
    <Panel title="Dar trabalho ao banco" icon={<Zap className="size-4 text-flame" />} hint={`Chaves no formato usuario:0000001. ${count(nextId)} já foram criadas.${sync ? " Com fsync a cada escrita." : ""}`}>
      <div className="grid grid-cols-2 gap-2">
        <Action disabled={busy} onClick={load("new", 5000, "chaves novas")} title="+5.000 novas" sub="enche a memtable e força gravações" />
        <Action disabled={busy} onClick={load("new", 50000, "chaves novas")} title="+50.000 novas" sub="faz os níveis de baixo crescerem" />
        <Action disabled={busy || nextId === 0} onClick={load("overwrite", 10000, "sobrescritas")} title="Sobrescrever 10.000" sub="versões antigas viram lixo até compactar" />
        <Action disabled={busy || nextId === 0} onClick={load("delete", 5000, "remoções")} title="Apagar 5.000" sub="cada remoção é uma escrita: uma marca" />
      </div>
      <button
        type="button"
        disabled={busy}
        onClick={go(async () => {
          const r = await api.crash();
          return `Processo morto sem aviso, com ${count(r.inMemory)} entradas só na memória. Reaberto em ${r.millis.toFixed(0)} ms: o log devolveu tudo. Veja o evento de recuperação.`;
        })}
        className="mt-3 flex w-full items-center justify-center gap-2 rounded-lg border border-bad/60 bg-bad/10 py-2.5 text-[13px] font-bold text-bad transition-colors hover:bg-bad/20 disabled:opacity-40"
      >
        <Skull className="size-4" /> Matar o processo agora
      </button>
    </Panel>
  );
}

function Action({ title, sub, ...rest }: React.ButtonHTMLAttributes<HTMLButtonElement> & { title: string; sub: string }) {
  return (
    <button type="button" {...rest} className="rounded-lg border border-line bg-ash-2 px-3 py-2.5 text-left transition-colors hover:border-ember disabled:opacity-40">
      <div className="text-[13px] font-bold">{title}</div>
      <div className="mt-0.5 text-[11px] leading-snug text-ink-3">{sub}</div>
    </button>
  );
}

const inputClass = "mono h-9 w-full min-w-0 rounded-lg border border-line bg-coal px-3 text-[13px] placeholder:text-ink-3 focus:border-ember focus:outline-none";

function Read({ run, nextId }: { run: Run; nextId: number }) {
  const [key, setKey] = useState("usuario:0000001");
  const [value, setValue] = useState("");
  const [result, setResult] = useState<{ found: boolean; value: string; steps: Step[]; micros: number } | null>(null);
  const read = (e?: FormEvent) => {
    e?.preventDefault();
    if (key) run(async () => setResult(await api.get(key)));
  };
  return (
    <Panel title="Ler, escrever, apagar" icon={<Search className="size-4 text-flame" />} hint="Ler mostra o caminho: onde o banco procurou, do dado mais novo ao mais antigo, e o que cada lugar respondeu.">
      <form onSubmit={read} className="flex gap-2">
        <input value={key} onChange={(e) => setKey(e.target.value)} placeholder="chave" aria-label="Chave" className={inputClass} spellCheck={false} />
        <button type="submit" className="h-9 shrink-0 rounded-lg bg-ember px-3.5 text-[13px] font-bold text-coal hover:bg-flame">
          Ler
        </button>
      </form>
      <div className="mt-2 flex gap-2">
        <input value={value} onChange={(e) => setValue(e.target.value)} placeholder="valor" aria-label="Valor" className={inputClass} spellCheck={false} />
        <button type="button" disabled={!key} onClick={() => run(async () => (await api.put(key, value), read(), undefined))} className="h-9 shrink-0 rounded-lg border border-line px-3 text-[13px] font-bold hover:border-ember disabled:opacity-40">
          Gravar
        </button>
        <button type="button" disabled={!key} aria-label="Apagar a chave" title="Apagar a chave" onClick={() => run(async () => (await api.del(key), read(), undefined))} className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-line text-ink-2 hover:border-bad hover:text-bad disabled:opacity-40">
          <Trash2 className="size-4" />
        </button>
      </div>
      <div className="mt-2 flex flex-wrap gap-1.5 text-[11px]">
        <span className="text-ink-3">tente:</span>
        {[["uma antiga", "usuario:0000001"], ["uma recente", `usuario:${String(Math.max(1, nextId)).padStart(7, "0")}`], ["uma que não existe", "usuario:0000001x"]].map(([label, k]) => (
          <button key={label} type="button" onClick={() => (setKey(k!), run(async () => setResult(await api.get(k!))))} className="rounded-full border border-line px-2 py-px font-semibold text-ink-2 hover:border-ember hover:text-ink">
            {label}
          </button>
        ))}
      </div>

      {result && (
        <div className="rise mt-4">
          <div className={clsx("rounded-lg border px-3 py-2", result.found ? "border-ok/40 bg-ok/10" : "border-line bg-coal")}>
            <div className="label">{result.found ? "valor" : "não encontrada"}</div>
            {result.found && <div className="mono mt-0.5 text-[12.5px] break-all">{result.value || "(vazio)"}</div>}
          </div>
          <ol className="mt-3 space-y-1.5">
            {result.steps.map((st, i) => {
              const skipped = st.result.includes("Bloom");
              const hit = st.result.includes("achou");
              return (
                <li key={i} className="flex items-start gap-2.5 text-[12.5px]">
                  <span className="mono mt-px w-4 shrink-0 text-right text-ink-3">{i + 1}</span>
                  <span className="mono w-28 shrink-0 font-bold" style={{ color: st.level < 0 ? "var(--color-hot-0)" : heat[st.level] }}>
                    {st.level < 0 ? st.where : `nível ${st.level} · ${st.table}`}
                  </span>
                  <span className={clsx("leading-snug", hit ? "font-semibold text-ok" : skipped ? "text-ink-3" : "text-ink-2")}>{st.result}</span>
                </li>
              );
            })}
          </ol>
          <p className="mono mt-2 text-[11px] text-ink-3">
            {result.steps.filter((s) => s.result.includes("leu um bloco")).length} leitura(s) de disco · {result.micros} µs
          </p>
        </div>
      )}
    </Panel>
  );
}

function Explorer({ rev }: { rev: number }) {
  const [prefix, setPrefix] = useState("usuario:");
  const [pairs, setPairs] = useState<{ key: string; value: string }[]>([]);
  useEffect(() => {
    const t = setTimeout(() => api.scan(prefix).then(setPairs, () => {}), 120);
    return () => clearTimeout(t);
  }, [prefix, rev]);
  return (
    <Panel title="Varrer em ordem" hint="As chaves ficam ordenadas em todos os níveis, então percorrer um intervalo é fundir várias listas ordenadas: a memtable e cada arquivo.">
      <input value={prefix} onChange={(e) => setPrefix(e.target.value)} placeholder="prefixo" aria-label="Prefixo" className={inputClass} spellCheck={false} />
      <ul className="mt-3 max-h-64 space-y-1 overflow-y-auto">
        {pairs.length === 0 && <li className="text-[12px] text-ink-3">Nenhuma chave com esse prefixo.</li>}
        {pairs.map((p) => (
          <li key={p.key} className="mono flex gap-3 text-[11.5px]">
            <span className="shrink-0 font-bold text-flame">{p.key}</span>
            <span className="truncate text-ink-2">{p.value}</span>
          </li>
        ))}
      </ul>
      {pairs.length >= 59 && <p className="mt-2 text-[11px] text-ink-3">Mostrando as primeiras 60.</p>}
    </Panel>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
