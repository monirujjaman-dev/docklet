<script lang="ts">
  import { onMount } from "svelte";
  import { api } from "../lib/api";
  import { bytes } from "../lib/format";

  type Overview = {
    os: { id: string; versionId: string; prettyName: string; systemd: boolean; supported: boolean };
    hostname: string; kernel: string; arch: string; uptimeSec: number; timezone: string; localTime: string; cpus: number;
    memTotal: number; memAvailable: number; swapTotal: number; swapUsed: number; swappiness: number; swapFileBytes: number;
    diskTotal: number; diskUsed: number; diskAvailable: number; rebootRequired: boolean;
  };
  type Port = { proto: string; address: string; port: number; process?: string; pid?: number; container?: string; owner?: string };
  type Proc = { pid: number; ppid: number; user: string; cpu: number; mem: number; rss: number; elapsedSec: number; comm: string; args: string; protected: boolean };
  type Unit = { name: string; load: string; active: string; sub: string; description: string; enabled: string; protected?: string };
  type Item = { id: string; name: string; size: number; detail?: string };
  type Category = { key: string; label: string; size: number; items?: Item[]; optIn?: boolean; note?: string };

  let tab = $state<"overview" | "cleanup" | "ports" | "processes" | "services">("overview");
  let o = $state<Overview | null>(null);
  let error = $state("");
  let msg = $state("");
  let busy = $state("");

  async function run(label: string, fn: () => Promise<unknown>, done?: string) {
    error = msg = "";
    busy = label;
    try {
      await fn();
      if (done) msg = done;
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = "";
    }
  }
  const loadOverview = () => run("load", async () => (o = await api<Overview>("/server/overview")));

  // Overview edits
  let hostname = $state("");
  let tz = $state("");
  let zones = $state<string[]>([]);
  let swapMb = $state("");
  let swappiness = $state("");
  function fill() {
    if (!o) return;
    hostname = o.hostname;
    tz = o.timezone;
    swapMb = o.swapFileBytes ? String(Math.round(o.swapFileBytes / 1048576)) : String(Math.min(4096, Math.max(1024, Math.round(o.memTotal / 1048576))));
    swappiness = String(o.swappiness);
  }
  const put = (path: string, json: unknown, done: string) =>
    run(path, async () => { o = await api<Overview>(`/server/${path}`, { method: "PUT", json }); fill(); }, done);

  // Reboot
  let rebootOpen = $state(false);
  let rebootName = $state("");
  let rebootPw = $state("");
  let rebooting = $state(false);
  async function reboot(e: SubmitEvent) {
    e.preventDefault();
    await run("reboot", async () => {
      await api("/server/reboot", { json: { hostname: rebootName.trim(), password: rebootPw } });
      rebootOpen = false;
      rebooting = true;
      const before = o?.uptimeSec ?? 0;
      await new Promise((r) => setTimeout(r, 15000));
      for (let i = 0; i < 100; i++) {
        try {
          const n = await api<Overview>("/server/overview");
          if (n.uptimeSec < before || n.uptimeSec < 300) {
            o = n;
            rebooting = false;
            msg = "The server is back online.";
            return;
          }
        } catch {
          /* still down */
        }
        await new Promise((r) => setTimeout(r, 3000));
      }
      rebooting = false;
      error = "The server hasn't come back after 5 minutes. Check it at your provider's console.";
    });
  }

  // Cleanup
  let cats = $state<Category[] | null>(null);
  let pick = $state<Record<string, boolean>>({});
  let vols = $state<Record<string, boolean>>({});
  let cleanResult = $state("");
  const loadCleanup = () =>
    run("cleanup-load", async () => {
      cats = (await api<{ categories: Category[] }>("/server/cleanup")).categories;
      for (const c of cats) if (!(c.key in pick)) pick[c.key] = !c.optIn && c.size > 0;
    });
  const selectedSize = $derived(
    (cats ?? []).reduce((n, c) => n + (c.optIn ? (c.items ?? []).filter((i) => vols[i.id]).reduce((m, i) => m + i.size, 0) : pick[c.key] ? c.size : 0), 0),
  );
  function clean() {
    const categories = (cats ?? []).filter((c) => (c.optIn ? (c.items ?? []).some((i) => vols[i.id]) : pick[c.key])).map((c) => c.key);
    const volumes = Object.keys(vols).filter((k) => vols[k]);
    if (volumes.length && !confirm(`Delete ${volumes.length} volume(s)? Their data is lost: ${volumes.join(", ")}`)) return;
    run("clean", async () => {
      const r = await api<{ freedBytes: number; results: { key: string; removed: number; errors?: string[] }[] }>("/server/cleanup", { json: { categories, volumes } });
      const errs = r.results.flatMap((x) => x.errors ?? []);
      cleanResult = `Freed ${bytes(Math.max(0, r.freedBytes))} of disk space.` + (errs.length ? ` ${errs.length} item(s) could not be removed: ${errs.slice(0, 3).join("; ")}` : "");
      vols = {};
      cats = (await api<{ categories: Category[] }>("/server/cleanup")).categories;
      await loadOverview();
    });
  }

  // Ports, processes, services
  let ports = $state<Port[]>([]);
  let procs = $state<Proc[]>([]);
  let units = $state<Unit[]>([]);
  let unitFilter = $state("");
  let showAll = $state(false);
  let logs = $state<{ unit: string; text: string } | null>(null);
  const loadPorts = () => run("ports", async () => (ports = (await api<{ ports: Port[] }>("/server/ports")).ports));
  const loadProcs = () => run("procs", async () => (procs = (await api<{ processes: Proc[] }>("/server/processes")).processes));
  const loadUnits = () => run("units", async () => (units = (await api<{ services: Unit[] }>("/server/services")).services));
  function signal(p: Proc, sig: "TERM" | "KILL") {
    if (!confirm(`Send ${sig} to ${p.comm} (pid ${p.pid})?`)) return;
    run("sig", async () => { await api(`/server/processes/${p.pid}/signal`, { json: { signal: sig } }); await new Promise((r) => setTimeout(r, 500)); await loadProcs(); });
  }
  function unitAction(u: Unit, action: string) {
    if ((action === "stop" || action === "disable") && !confirm(`${action} ${u.name}?`)) return;
    run("unit", async () => { await api(`/server/services/${encodeURIComponent(u.name)}/${action}`, { method: "POST" }); await loadUnits(); }, `${u.name}: ${action} done`);
  }
  const showLogs = (u: Unit) => run("logs", async () => (logs = { unit: u.name, text: (await api<{ logs: string }>(`/server/services/${encodeURIComponent(u.name)}/logs`)).logs }));
  const visibleUnits = $derived(
    units.filter((u) => (showAll || u.load !== "not-loaded") && (!unitFilter || u.name.includes(unitFilter) || u.description.toLowerCase().includes(unitFilter.toLowerCase()))),
  );

  function select(t: typeof tab) {
    tab = t;
    if (t === "cleanup" && !cats) loadCleanup();
    if (t === "ports") loadPorts();
    if (t === "processes") loadProcs();
    if (t === "services" && !units.length) loadUnits();
  }

  onMount(async () => {
    await loadOverview();
    fill();
    api<{ timezones: string[] }>("/server/timezones").then((r) => (zones = r.timezones)).catch(() => {});
  });

  const up = (s: number) => {
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
  };
  const pct = (a: number, b: number) => (b ? Math.round((a / b) * 100) : 0);
  const tabs = [["overview", "Overview"], ["cleanup", "Disk cleanup"], ["ports", "Listening ports"], ["processes", "Processes"], ["services", "Services"]] as const;
  const input = "rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm outline-none focus:border-emerald-500";
  const btn = "rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50";
  const card = "rounded-lg border border-neutral-800 p-4 space-y-3";
</script>

<div class="flex items-start justify-between gap-4 mb-2">
  <div>
    <h1 class="text-xl font-semibold">Server</h1>
    {#if o}<p class="text-sm text-neutral-400">{o.hostname} · {o.os.prettyName} · up {up(o.uptimeSec)}</p>{/if}
  </div>
  {#if o?.os.supported}
    <button onclick={() => { rebootOpen = true; rebootName = ""; rebootPw = ""; }} disabled={rebooting} class="rounded-md border border-red-900 text-red-300 hover:bg-red-950 px-3 py-1.5 text-sm">Reboot…</button>
  {/if}
</div>

{#if o && !o.os.supported}
  <p class="text-sm text-amber-300 mb-3">This host ({o.os.prettyName || "unknown OS"}{o.os.systemd ? "" : ", no systemd"}) is shown read-only: Docklet changes Ubuntu and Debian servers with systemd.</p>
{/if}
{#if o?.rebootRequired}<p class="text-sm text-amber-300 mb-3">Updates installed on the server need a reboot.</p>{/if}
{#if rebooting}<p class="text-sm text-amber-300 mb-3">Rebooting… waiting for the server to come back (usually 30-90 s). Your apps restart automatically.</p>{/if}
{#if error}<p class="text-sm text-red-400 mb-3">{error}</p>{/if}
{#if msg}<p class="text-sm text-emerald-300 mb-3">{msg}</p>{/if}

{#if rebootOpen && o}
  <form onsubmit={reboot} class="{card} max-w-md mb-6 border-red-900">
    <p class="text-sm">Reboot <strong>{o.hostname}</strong>? Every app and database stops for about a minute and starts again on its own.</p>
    <label class="block text-sm">Type the host name to confirm<input bind:value={rebootName} class="mt-1 w-full {input} font-mono" /></label>
    <label class="block text-sm">Your password<input type="password" bind:value={rebootPw} autocomplete="current-password" class="mt-1 w-full {input}" /></label>
    <div class="flex gap-2">
      <button disabled={rebootName !== o.hostname || !rebootPw || !!busy} class="rounded-md bg-red-700 hover:bg-red-600 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">Reboot now</button>
      <button type="button" onclick={() => (rebootOpen = false)} class="px-3 py-1.5 text-sm text-neutral-400">Cancel</button>
    </div>
  </form>
{/if}

<nav class="flex gap-1 border-b border-neutral-800 mb-6">
  {#each tabs as [id, label]}
    <button onclick={() => select(id)} class="px-3 py-2 text-sm -mb-px border-b-2 {tab === id ? 'border-emerald-500 text-white' : 'border-transparent text-neutral-400 hover:text-white'}">{label}</button>
  {/each}
</nav>

{#if !o}
  <p class="text-sm text-neutral-500">{busy ? "Loading…" : ""}</p>
{:else if tab === "overview"}
  <div class="grid gap-4 lg:grid-cols-2 max-w-6xl">
    <section class={card}>
      <h2 class="font-medium">System</h2>
      <dl class="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
        <dt class="text-neutral-400">OS</dt><dd>{o.os.prettyName}</dd>
        <dt class="text-neutral-400">Kernel</dt><dd class="font-mono text-xs pt-0.5">{o.kernel} ({o.arch})</dd>
        <dt class="text-neutral-400">CPU</dt><dd>{o.cpus} cores</dd>
        <dt class="text-neutral-400">Memory</dt><dd>{bytes(o.memTotal - o.memAvailable)} of {bytes(o.memTotal)} used</dd>
        <dt class="text-neutral-400">Disk (/)</dt><dd>{bytes(o.diskUsed)} of {bytes(o.diskTotal)} ({pct(o.diskUsed, o.diskTotal)}%), {bytes(o.diskAvailable)} free</dd>
        <dt class="text-neutral-400">Time</dt><dd>{o.localTime}</dd>
        <dt class="text-neutral-400">Uptime</dt><dd>{up(o.uptimeSec)}</dd>
      </dl>
    </section>

    <section class={card}>
      <h2 class="font-medium">Host name & time zone</h2>
      <div class="flex gap-2 items-end">
        <label class="text-sm flex-1">Host name<input bind:value={hostname} class="mt-1 w-full {input} font-mono" /></label>
        <button onclick={() => put("hostname", { hostname }, "Host name changed.")} disabled={!o.os.supported || hostname === o.hostname || !!busy} class={btn}>Save</button>
      </div>
      <div class="flex gap-2 items-end">
        <label class="text-sm flex-1">Time zone
          <input bind:value={tz} list="tz-list" class="mt-1 w-full {input}" />
          <datalist id="tz-list">{#each zones as z}<option value={z}></option>{/each}</datalist></label>
        <button onclick={() => put("timezone", { timezone: tz }, "Time zone changed.")} disabled={!o.os.supported || tz === o.timezone || !!busy} class={btn}>Save</button>
      </div>
      <p class="text-xs text-neutral-500">Cron jobs keep their own time zones; this sets the server clock's zone (logs, system timers).</p>
    </section>

    <section class={card}>
      <h2 class="font-medium">Swap</h2>
      <p class="text-sm">
        {#if o.swapTotal}{bytes(o.swapUsed)} of {bytes(o.swapTotal)} used{o.swapFileBytes ? ` · /swapfile ${bytes(o.swapFileBytes)}` : ""}{:else}No swap. On small servers (1-2 GB RAM) swap keeps builds and databases from being killed when memory runs out.{/if}
      </p>
      <div class="flex flex-wrap gap-2 items-end">
        <label class="text-sm">Swap file size (MB)<input bind:value={swapMb} inputmode="numeric" class="mt-1 w-32 {input}" /></label>
        <button onclick={() => put("swap", { sizeMb: Number(swapMb) }, "Swap updated.")} disabled={!o.os.supported || !!busy} class={btn}>{o.swapFileBytes ? "Resize" : "Create"}</button>
        {#if o.swapFileBytes}
          <button onclick={() => confirm("Remove /swapfile?") && put("swap", { sizeMb: 0 }, "Swap removed.")} disabled={!o.os.supported || !!busy} class="{btn} text-red-300">Remove</button>
        {/if}
      </div>
      <div class="flex flex-wrap gap-2 items-end">
        <label class="text-sm">Swappiness (0-100)<input bind:value={swappiness} inputmode="numeric" class="mt-1 w-24 {input}" /></label>
        <button onclick={() => put("swappiness", { value: Number(swappiness) }, "Swappiness updated.")} disabled={!o.os.supported || Number(swappiness) === o.swappiness || !!busy} class={btn}>Save</button>
      </div>
      <p class="text-xs text-neutral-500">10 is a good value for servers: swap is used only under memory pressure. Default is 60. Both settings survive reboots.</p>
    </section>
  </div>
{:else if tab === "cleanup"}
  <section class="max-w-4xl space-y-4">
    <div class="flex items-center justify-between">
      <p class="text-sm text-neutral-400">{bytes(o.diskAvailable)} free of {bytes(o.diskTotal)}.</p>
      <button onclick={loadCleanup} disabled={!!busy} class={btn}>{busy === "cleanup-load" ? "Scanning…" : "Rescan"}</button>
    </div>
    {#if cats}
      {#each cats as c (c.key)}
        <div class="rounded-md border border-neutral-800 p-3 space-y-2">
          <label class="flex items-center justify-between gap-3 text-sm">
            <span class="flex items-center gap-2">
              {#if !c.optIn}<input type="checkbox" bind:checked={pick[c.key]} disabled={c.size === 0} />{/if}
              <span class="font-medium">{c.label}</span>
            </span>
            <span class="tabular-nums text-neutral-300">{bytes(c.size)}</span>
          </label>
          {#if c.note}<p class="text-xs text-neutral-500">{c.note}</p>{/if}
          {#if c.items?.length}
            <div class="max-h-48 overflow-y-auto text-xs divide-y divide-neutral-900">
              {#each c.items as it (it.id)}
                <label class="flex items-center justify-between gap-2 py-1">
                  <span class="flex items-center gap-2 min-w-0">
                    {#if c.optIn}<input type="checkbox" bind:checked={vols[it.id]} />{/if}
                    <span class="font-mono truncate">{it.name}</span>{#if it.detail}<span class="text-neutral-500 truncate">{it.detail}</span>{/if}
                  </span>
                  <span class="tabular-nums text-neutral-500 shrink-0">{bytes(it.size)}</span>
                </label>
              {/each}
            </div>
          {/if}
        </div>
      {/each}
      <div class="flex items-center gap-3">
        <button onclick={clean} disabled={!!busy || selectedSize === 0} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-4 py-2 text-sm font-medium">
          {busy === "clean" ? "Cleaning…" : `Free about ${bytes(selectedSize)}`}</button>
        {#if cleanResult}<p class="text-sm text-emerald-300">{cleanResult}</p>{/if}
      </div>
    {:else}
      <p class="text-sm text-neutral-500">Scanning…</p>
    {/if}
  </section>
{:else if tab === "ports"}
  <section class="max-w-5xl space-y-3">
    <div class="flex justify-between items-center">
      <p class="text-sm text-neutral-400">Sockets listening on this server. Ports bound to 127.0.0.1 or ::1 are reachable only from the server itself.</p>
      <button onclick={loadPorts} disabled={!!busy} class={btn}>Refresh</button>
    </div>
    <div class="overflow-x-auto rounded-md border border-neutral-800">
      <table class="w-full text-sm">
        <thead class="bg-neutral-900 text-neutral-400 text-left"><tr><th class="px-3 py-2 font-medium">Port</th><th class="px-3 py-2 font-medium">Address</th><th class="px-3 py-2 font-medium">Process</th><th class="px-3 py-2 font-medium">Container / owner</th></tr></thead>
        <tbody>
          {#each ports as p}
            <tr class="border-t border-neutral-800">
              <td class="px-3 py-1.5 font-mono">{p.port}/{p.proto}</td>
              <td class="px-3 py-1.5 font-mono text-xs {p.address.startsWith('127.') || p.address === '::1' ? 'text-neutral-500' : 'text-amber-200'}">{p.address}</td>
              <td class="px-3 py-1.5">{p.process ?? ""}{#if p.pid}<span class="text-xs text-neutral-500"> · {p.pid}</span>{/if}</td>
              <td class="px-3 py-1.5 text-neutral-300">{p.owner ?? ""}{#if p.container && p.container !== p.owner}<span class="text-xs text-neutral-500"> {p.container}</span>{/if}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>
{:else if tab === "processes"}
  <section class="max-w-6xl space-y-3">
    <div class="flex justify-between items-center">
      <p class="text-sm text-neutral-400">Top 100 by CPU. Docker, Docklet, Caddy, SSH and system processes are protected.</p>
      <button onclick={loadProcs} disabled={!!busy} class={btn}>Refresh</button>
    </div>
    <div class="overflow-x-auto rounded-md border border-neutral-800">
      <table class="w-full text-sm">
        <thead class="bg-neutral-900 text-neutral-400 text-left"><tr>
          <th class="px-3 py-2 font-medium">PID</th><th class="px-3 py-2 font-medium">User</th><th class="px-3 py-2 font-medium text-right">CPU %</th>
          <th class="px-3 py-2 font-medium text-right">Memory</th><th class="px-3 py-2 font-medium">Command</th><th></th></tr></thead>
        <tbody>
          {#each procs as p (p.pid)}
            <tr class="border-t border-neutral-800">
              <td class="px-3 py-1.5 font-mono text-xs">{p.pid}</td>
              <td class="px-3 py-1.5 text-xs">{p.user}</td>
              <td class="px-3 py-1.5 text-right tabular-nums">{p.cpu.toFixed(1)}</td>
              <td class="px-3 py-1.5 text-right tabular-nums">{bytes(p.rss)}</td>
              <td class="px-3 py-1.5 font-mono text-xs max-w-md truncate" title={p.args}>{p.args || p.comm}</td>
              <td class="px-3 py-1.5 text-right whitespace-nowrap">
                {#if !p.protected}
                  <button onclick={() => signal(p, "TERM")} class="text-xs text-neutral-300 hover:underline mr-2">Stop</button>
                  <button onclick={() => signal(p, "KILL")} class="text-xs text-red-300 hover:underline">Kill</button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>
{:else}
  <section class="max-w-6xl space-y-3">
    <div class="flex flex-wrap gap-3 items-center justify-between">
      <input bind:value={unitFilter} placeholder="Filter services…" class="{input} w-64" />
      <span class="flex items-center gap-3">
        <label class="text-xs text-neutral-400 flex items-center gap-1"><input type="checkbox" bind:checked={showAll} /> include not loaded</label>
        <button onclick={loadUnits} disabled={!!busy} class={btn}>Refresh</button>
      </span>
    </div>
    <div class="overflow-x-auto rounded-md border border-neutral-800">
      <table class="w-full text-sm">
        <thead class="bg-neutral-900 text-neutral-400 text-left"><tr><th class="px-3 py-2 font-medium">Service</th><th class="px-3 py-2 font-medium">State</th><th class="px-3 py-2 font-medium">On boot</th><th></th></tr></thead>
        <tbody>
          {#each visibleUnits as u (u.name)}
            <tr class="border-t border-neutral-800 align-top">
              <td class="px-3 py-1.5"><span class="font-mono text-xs">{u.name}</span><div class="text-xs text-neutral-500">{u.description}</div></td>
              <td class="px-3 py-1.5 text-xs {u.active === 'active' ? 'text-emerald-300' : u.active === 'failed' ? 'text-red-300' : 'text-neutral-400'}">{u.active} ({u.sub})</td>
              <td class="px-3 py-1.5 text-xs text-neutral-400">{u.enabled}</td>
              <td class="px-3 py-1.5 text-right whitespace-nowrap space-x-2 text-xs">
                <button onclick={() => showLogs(u)} class="text-neutral-300 hover:underline">Logs</button>
                {#if !u.protected?.includes("needed by")}
                  {#if u.active === "active"}<button onclick={() => unitAction(u, "restart")} disabled={!!busy} class="text-neutral-300 hover:underline">Restart</button>{/if}
                  {#if !u.protected}
                    {#if u.active === "active"}<button onclick={() => unitAction(u, "stop")} disabled={!!busy} class="text-neutral-300 hover:underline">Stop</button>
                    {:else}<button onclick={() => unitAction(u, "start")} disabled={!!busy} class="text-neutral-300 hover:underline">Start</button>{/if}
                    {#if u.enabled === "enabled"}<button onclick={() => unitAction(u, "disable")} disabled={!!busy} class="text-neutral-400 hover:underline">Disable</button>
                    {:else if u.enabled === "disabled"}<button onclick={() => unitAction(u, "enable")} disabled={!!busy} class="text-neutral-400 hover:underline">Enable</button>{/if}
                  {/if}
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    {#if logs}
      <div class="rounded-md border border-neutral-800 p-3">
        <div class="flex justify-between text-sm mb-2"><span class="font-mono">{logs.unit}</span><button onclick={() => (logs = null)} class="text-xs text-neutral-400">Close</button></div>
        <pre class="text-xs font-mono whitespace-pre-wrap max-h-96 overflow-y-auto text-neutral-300">{logs.text || "(no log lines)"}</pre>
      </div>
    {/if}
  </section>
{/if}
