<script lang="ts">
  import { onMount } from "svelte";
  import { api, type Project } from "../lib/api";
  import { subscribe, type MetricsStatus, type Sample } from "../lib/metrics";
  import { bytes, percent, rate } from "../lib/format";
  import { router } from "../lib/router.svelte";
  import MetricsPanel from "../lib/MetricsPanel.svelte";

  let status = $state<MetricsStatus | null>(null);
  let host = $state<Sample | null>(null);
  let apps = $state<Record<string, Sample>>({});
  let names = $state<Record<string, { name: string; project: string }>>({});
  let error = $state("");

  type Sort = "cpu" | "memUsed" | "net";
  let sortBy = $state<Sort>("cpu");

  const rows = $derived(
    Object.entries(apps)
      .map(([id, s]) => ({ id, s, ...(names[id] ?? { name: `app ${id}`, project: "" }) }))
      .sort((a, b) =>
        sortBy === "net" ? b.s.netRx + b.s.netTx - (a.s.netRx + a.s.netTx) : (b.s[sortBy] as number) - (a.s[sortBy] as number),
      ),
  );

  async function loadNames() {
    const res = await api<{ projects: Project[] }>("/projects");
    const n: typeof names = {};
    for (const p of res.projects) for (const a of p.apps) n[String(a.id)] = { name: a.name, project: p.name };
    names = n;
  }

  onMount(() => {
    api<MetricsStatus>("/metrics/status")
      .then((s) => (status = s))
      .catch((e) => (error = e instanceof Error ? e.message : String(e)));
    loadNames();
    const unsub = subscribe((snap) => {
      if (snap.host) host = snap.host;
      if (snap.apps) {
        apps = snap.apps;
        if (Object.keys(snap.apps).some((id) => !names[id])) loadNames();
      }
    });
    return unsub;
  });

  const diskPct = $derived(host?.diskTotal ? ((host.diskUsed ?? 0) / host.diskTotal) * 100 : 0);
  const memPct = $derived(host?.memLimit ? (host.memUsed / host.memLimit) * 100 : 0);
</script>

<div class="flex items-center justify-between mb-6">
  <h1 class="text-xl font-semibold">Monitoring</h1>
  {#if status}
    <span class="text-xs text-neutral-500">
      Sampling every {status.intervalMs / 1000}s · container source: {status.containerSource} · history kept {status.retentionDays} days
    </span>
  {/if}
</div>

{#if error}<p class="text-sm text-red-400 mb-4">{error}</p>{/if}

<div class="grid gap-3 grid-cols-2 lg:grid-cols-4 mb-6">
  {#each [
    { label: "CPU", value: host ? percent(host.cpu) : "–", sub: status?.cores ? `${status.cores} cores` : "", pct: host?.cpu ?? 0 },
    { label: "Memory", value: host ? bytes(host.memUsed) : "–", sub: host ? `of ${bytes(host.memLimit)}` : "", pct: memPct },
    { label: "Disk", value: host?.diskTotal ? bytes(host.diskUsed ?? 0) : "–", sub: host?.diskTotal ? `of ${bytes(host.diskTotal)}` : "", pct: diskPct },
    { label: "Network", value: host ? rate(host.netRx + host.netTx) : "–", sub: host ? `↓ ${rate(host.netRx)} · ↑ ${rate(host.netTx)}` : "", pct: -1 },
  ] as tile}
    <div class="rounded-lg border border-neutral-800 p-4">
      <p class="text-xs text-neutral-400">{tile.label}</p>
      <p class="text-2xl font-semibold tabular-nums mt-1">{tile.value}</p>
      <p class="text-xs text-neutral-500 mt-0.5 h-4">{tile.sub}</p>
      {#if tile.pct >= 0}
        <div class="mt-3 h-1.5 rounded-full bg-neutral-800 overflow-hidden" role="meter" aria-valuenow={Math.round(tile.pct)} aria-valuemin="0" aria-valuemax="100" aria-label="{tile.label} usage">
          <div class="h-full rounded-full bg-[#3987e5]" style="width: {Math.min(100, tile.pct)}%"></div>
        </div>
        {#if tile.pct >= 90}<p class="text-xs text-amber-300 mt-1">⚠ above 90%</p>{/if}
      {/if}
    </div>
  {/each}
</div>

<h2 class="font-medium mb-3">Host</h2>
<MetricsPanel scope="host" cores={status?.cores ?? 0} />

<div class="flex items-center justify-between mt-8 mb-3">
  <h2 class="font-medium">Apps</h2>
  <label class="text-xs text-neutral-400 flex items-center gap-2"
    >Sort by
    <select bind:value={sortBy} class="rounded bg-neutral-900 border border-neutral-700 px-2 py-1">
      <option value="cpu">CPU</option>
      <option value="memUsed">Memory</option>
      <option value="net">Network</option>
    </select>
  </label>
</div>
<div class="overflow-x-auto rounded-md border border-neutral-800">
  <table class="w-full text-sm">
    <thead class="bg-neutral-900 text-neutral-400 text-left">
      <tr>
        <th class="px-3 py-2 font-medium">App</th>
        <th class="px-3 py-2 font-medium text-right">CPU</th>
        <th class="px-3 py-2 font-medium text-right">Memory</th>
        <th class="px-3 py-2 font-medium text-right">Net ↓</th>
        <th class="px-3 py-2 font-medium text-right">Net ↑</th>
        <th class="px-3 py-2 font-medium text-right">Disk I/O</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as r (r.id)}
        <tr class="border-t border-neutral-800 hover:bg-neutral-900 cursor-pointer" onclick={() => router.go(`/apps/${r.id}`)}>
          <td class="px-3 py-2"><span class="text-neutral-500">{r.project}/</span>{r.name}</td>
          <td class="px-3 py-2 text-right tabular-nums">{percent(r.s.cpu)}</td>
          <td class="px-3 py-2 text-right tabular-nums">
            {bytes(r.s.memUsed)}{#if r.s.memLimit && r.s.memLimit < (host?.memLimit ?? Infinity)}<span class="text-neutral-500"> / {bytes(r.s.memLimit)}</span>{/if}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">{rate(r.s.netRx)}</td>
          <td class="px-3 py-2 text-right tabular-nums">{rate(r.s.netTx)}</td>
          <td class="px-3 py-2 text-right tabular-nums">{rate(r.s.blkRead + r.s.blkWrite)}</td>
        </tr>
      {:else}
        <tr><td colspan="6" class="px-3 py-6 text-center text-neutral-500">No running apps yet.</td></tr>
      {/each}
    </tbody>
  </table>
</div>
