<script lang="ts">
  import { onMount } from "svelte";
  import { api, type CronJob, type CronRun } from "./api";
  import StatusBadge from "./StatusBadge.svelte";

  let { appId, deployed }: { appId: number; deployed: boolean } = $props();

  let jobs = $state<CronJob[]>([]);
  let available = $state(true);
  let error = $state("");
  let selected = $state<number | null>(null);
  let runs = $state<CronRun[]>([]);
  let openRun = $state<CronRun | null>(null);

  async function load() {
    try {
      const res = await api<{ jobs: CronJob[]; available: boolean }>(`/apps/${appId}/cron`);
      jobs = res.jobs;
      available = res.available;
      if (selected !== null) await loadRuns(selected);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function loadRuns(id: number) {
    const res = await api<{ runs: CronRun[] }>(`/cron/${id}/runs?limit=50`);
    runs = res.runs;
    if (openRun) {
      const still = runs.find((r) => r.id === openRun!.id);
      if (still && (still.status !== openRun.status || openRun.status === "running")) await showRun(still.id);
    }
  }

  async function act(fn: () => Promise<unknown>) {
    error = "";
    try {
      await fn();
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function select(id: number) {
    selected = selected === id ? null : id;
    openRun = null;
    if (selected !== null) await loadRuns(selected);
  }

  async function showRun(id: number) {
    openRun = (await api<{ run: CronRun }>(`/cron-runs/${id}`)).run;
  }

  async function runNow(j: CronJob) {
    await act(async () => {
      const res = await api<{ runId: number; status: string; error?: string }>(`/cron/${j.id}/run`, { method: "POST" });
      if (res.status === "skipped") error = res.error ?? "Skipped: a run is already in progress.";
      selected = j.id;
      await loadRuns(j.id);
      await showRun(res.runId);
    });
  }

  function remove(j: CronJob) {
    if (!confirm(`Delete cron job "${j.name}" and its run history? A run in progress is stopped.`)) return;
    if (selected === j.id) selected = null;
    act(() => api(`/cron/${j.id}`, { method: "DELETE" }));
  }

  // New job form
  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  let showForm = $state(false);
  let form = $state({ name: "", schedule: "*/5 * * * *", timezone: browserTz, command: "", timeoutSec: "3600", concurrency: "skip" });
  let preview = $state<{ next: number[]; error: string }>({ next: [], error: "" });
  let previewTimer: ReturnType<typeof setTimeout> | undefined;

  $effect(() => {
    const { schedule, timezone } = form;
    clearTimeout(previewTimer);
    previewTimer = setTimeout(async () => {
      try {
        const res = await api<{ next: number[] }>("/cron/preview", { json: { schedule, timezone } });
        preview = { next: res.next, error: "" };
      } catch (err) {
        preview = { next: [], error: err instanceof Error ? err.message : String(err) };
      }
    }, 300);
  });

  async function create(e: SubmitEvent) {
    e.preventDefault();
    await act(async () => {
      await api(`/apps/${appId}/cron`, {
        json: { ...form, name: form.name.trim(), command: form.command.trim(), timeoutSec: Number(form.timeoutSec) },
      });
      showForm = false;
      form = { ...form, name: "", command: "" };
    });
  }

  onMount(() => {
    load();
    const t = setInterval(load, 3000);
    return () => clearInterval(t);
  });

  const presets = [
    ["Every minute", "* * * * *"],
    ["Every 5 min", "*/5 * * * *"],
    ["Hourly", "0 * * * *"],
    ["Daily 03:00", "0 3 * * *"],
    ["Mondays 09:00", "0 9 * * 1"],
  ];
  const fmt = (ts?: number, tz?: string) =>
    ts ? new Date(ts * 1000).toLocaleString(undefined, tz ? { timeZone: tz, dateStyle: "medium", timeStyle: "short" } : undefined) : "";
  const dur = (ms: number) => (ms < 1000 ? `${ms} ms` : ms < 60000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`);
  const input = "w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm";
</script>

<section class="max-w-6xl space-y-4">
  <div class="flex items-center justify-between gap-4">
    <p class="text-sm text-neutral-400">
      Each run starts a one-off container from this app's current image, with its environment and project network, and
      removes it afterwards.
    </p>
    {#if available}
      <button onclick={() => (showForm = !showForm)} class="shrink-0 rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">
        {showForm ? "Cancel" : "New cron job"}
      </button>
    {/if}
  </div>
  {#if !available}<p class="text-sm text-amber-300">Cron jobs need Docker, which is not available on this server.</p>{/if}
  {#if !deployed}<p class="text-sm text-amber-300">Deploy the app first: cron runs use the image of its current deployment.</p>{/if}
  {#if error}<p class="text-sm text-red-400">{error}</p>{/if}

  {#if showForm}
    <form onsubmit={create} class="rounded-lg border border-neutral-800 p-4 space-y-4">
      <div class="grid gap-3 md:grid-cols-3">
        <label class="text-sm">Name<input bind:value={form.name} placeholder="nightly-report" class="mt-1 {input}" /></label>
        <label class="text-sm">Schedule<input bind:value={form.schedule} class="mt-1 {input} font-mono" /></label>
        <label class="text-sm">Time zone<input bind:value={form.timezone} class="mt-1 {input}" /></label>
      </div>
      <div class="flex flex-wrap gap-1.5">
        {#each presets as [label, expr]}
          <button type="button" onclick={() => (form.schedule = expr)} class="rounded-full border border-neutral-800 hover:border-neutral-600 px-2.5 py-0.5 text-xs text-neutral-300">{label}</button>
        {/each}
      </div>
      <p class="text-xs {preview.error ? 'text-red-400' : 'text-neutral-500'}">
        {#if preview.error}{preview.error}{:else if preview.next.length}Next: {preview.next.map((t) => fmt(t, form.timezone)).join(" · ")}{/if}
      </p>
      <label class="block text-sm">Command
        <input bind:value={form.command} placeholder="php artisan schedule:run" class="mt-1 {input} font-mono" />
        <span class="block text-xs text-neutral-500 mt-1">Runs without a shell; use sh -c '…' for pipes, &&, or $VARS.</span>
      </label>
      <div class="grid gap-3 md:grid-cols-3">
        <label class="text-sm">Timeout (seconds)<input bind:value={form.timeoutSec} inputmode="numeric" class="mt-1 {input}" /></label>
        <label class="text-sm md:col-span-2">If the previous run is still going
          <select bind:value={form.concurrency} class="mt-1 {input}">
            <option value="skip">Skip the new run (recommended)</option>
            <option value="allow">Start it anyway (runs overlap)</option>
          </select></label>
      </div>
      <button disabled={!!preview.error} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">Create</button>
    </form>
  {/if}

  <div class="overflow-x-auto rounded-md border border-neutral-800">
    <table class="w-full text-sm">
      <thead class="bg-neutral-900 text-neutral-400 text-left">
        <tr>
          <th class="px-3 py-2 font-medium">Job</th><th class="px-3 py-2 font-medium">Schedule</th>
          <th class="px-3 py-2 font-medium">Next run</th><th class="px-3 py-2 font-medium">Last run</th><th></th>
        </tr>
      </thead>
      <tbody>
        {#each jobs as j (j.id)}
          <tr class="border-t border-neutral-800 {selected === j.id ? 'bg-neutral-900/60' : ''}">
            <td class="px-3 py-2">
              <button onclick={() => select(j.id)} class="font-medium hover:underline">{j.name}</button>
              <div class="text-xs text-neutral-500 font-mono truncate max-w-xs" title={j.command}>{j.command}</div>
            </td>
            <td class="px-3 py-2"><span class="font-mono">{j.schedule}</span><div class="text-xs text-neutral-500">{j.timezone} · {j.concurrency === "skip" ? "no overlap" : "may overlap"}</div></td>
            <td class="px-3 py-2 whitespace-nowrap">{j.enabled ? fmt(j.nextRunAt) : "paused"}</td>
            <td class="px-3 py-2 whitespace-nowrap">
              {#if j.lastRun}<StatusBadge status={j.lastRun.status} /> <span class="text-xs text-neutral-500">{fmt(j.lastRun.startedAt)}</span>{:else}<span class="text-neutral-500">never</span>{/if}
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap space-x-2">
              <button onclick={() => runNow(j)} disabled={!deployed} class="text-xs rounded border border-neutral-700 hover:border-neutral-500 px-2 py-0.5 disabled:opacity-50">Run now</button>
              <button onclick={() => act(() => api(`/cron/${j.id}`, { method: "PATCH", json: { enabled: !j.enabled } }))} class="text-xs text-neutral-300 hover:underline">{j.enabled ? "Pause" : "Resume"}</button>
              <button onclick={() => remove(j)} class="text-xs text-neutral-500 hover:text-red-300">Delete</button>
            </td>
          </tr>
        {:else}
          <tr><td colspan="5" class="px-3 py-6 text-center text-neutral-500">No cron jobs.</td></tr>
        {/each}
      </tbody>
    </table>
  </div>

  {#if selected !== null}
    <div class="grid gap-4 lg:grid-cols-2">
      <div class="overflow-x-auto rounded-md border border-neutral-800 max-h-96 overflow-y-auto">
        <table class="w-full text-sm">
          <thead class="bg-neutral-900 text-neutral-400 text-left sticky top-0">
            <tr><th class="px-3 py-2 font-medium">Started</th><th class="px-3 py-2 font-medium">Status</th><th class="px-3 py-2 font-medium text-right">Duration</th><th class="px-3 py-2 font-medium">Trigger</th></tr>
          </thead>
          <tbody>
            {#each runs as r (r.id)}
              <tr onclick={() => showRun(r.id)} class="border-t border-neutral-800 cursor-pointer hover:bg-neutral-900 {openRun?.id === r.id ? 'bg-neutral-900' : ''}">
                <td class="px-3 py-1.5 whitespace-nowrap">{fmt(r.startedAt)}</td>
                <td class="px-3 py-1.5"><StatusBadge status={r.status} />{#if r.exitCode !== undefined && r.exitCode !== 0}<span class="text-xs text-neutral-500 ml-1">exit {r.exitCode}</span>{/if}</td>
                <td class="px-3 py-1.5 text-right tabular-nums">{r.status === "running" || r.status === "skipped" ? "" : dur(r.durationMs)}</td>
                <td class="px-3 py-1.5 text-neutral-400">{r.trigger}</td>
              </tr>
            {:else}
              <tr><td colspan="4" class="px-3 py-6 text-center text-neutral-500">No runs yet.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
      <div class="rounded-md border border-neutral-800 p-3 min-h-40">
        {#if openRun}
          <div class="flex items-center justify-between gap-2 mb-2 text-sm">
            <span>Run #{openRun.id} · {openRun.status}{openRun.exitCode !== undefined ? ` · exit ${openRun.exitCode}` : ""}</span>
            {#if openRun.status === "running"}
              <button onclick={() => act(() => api(`/cron-runs/${openRun!.id}/cancel`, { method: "POST" }))} class="text-xs rounded border border-red-900 text-red-300 hover:bg-red-950 px-2 py-0.5">Stop</button>
            {/if}
          </div>
          {#if openRun.error}<p class="text-xs text-red-300 mb-2">{openRun.error}</p>{/if}
          {#if openRun.status === "running"}
            <p class="text-xs text-neutral-500">Running… output is saved when the run ends.</p>
          {:else}
            <pre class="text-xs font-mono whitespace-pre-wrap break-all max-h-80 overflow-y-auto text-neutral-300">{openRun.logs || "(no output)"}</pre>
          {/if}
        {:else}
          <p class="text-sm text-neutral-500">Select a run to see its output.</p>
        {/if}
      </div>
    </div>
  {/if}
</section>
