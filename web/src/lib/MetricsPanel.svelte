<script lang="ts">
  import { onMount } from "svelte";
  import TimeChart from "./TimeChart.svelte";
  import { appendSample, loadSeries, subscribe, type Range, type Sample } from "./metrics";
  import { bytes, percent, rate } from "./format";

  let { scope, cores = 0 }: { scope: string; cores?: number } = $props();

  let range = $state<Range>("live");
  let samples = $state<Sample[]>([]);
  let error = $state("");

  const appId = $derived(scope.startsWith("app:") ? scope.slice(4) : "");
  const ts = $derived(samples.map((s) => s.ts));
  const memLimit = $derived(samples.length ? samples[samples.length - 1].memLimit : 0);

  async function load() {
    error = "";
    try {
      samples = await loadSeries(scope, range);
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  onMount(() => {
    const unsub = subscribe((snap) => {
      if (range !== "live") return;
      const s = appId ? snap.apps?.[appId] : snap.host;
      if (s) samples = appendSample(samples, s);
    });
    // Refresh history views once a minute (new one-minute point).
    const t = setInterval(() => range !== "live" && load(), 60_000);
    return () => {
      unsub();
      clearInterval(t);
    };
  });

  $effect(() => {
    void range;
    load();
  });

  const ranges: [Range, string][] = [
    ["live", "Live"],
    ["1h", "1 h"],
    ["6h", "6 h"],
    ["24h", "24 h"],
    ["7d", "7 d"],
  ];
  const cpuTitle = $derived(appId ? "CPU (% of one core)" : cores ? `CPU (${cores} cores)` : "CPU");
</script>

<div class="flex items-center justify-between mb-3">
  <div class="inline-flex rounded-md border border-neutral-800 p-0.5" role="group" aria-label="Time range">
    {#each ranges as [id, label]}
      <button
        onclick={() => (range = id)}
        aria-pressed={range === id}
        class="px-2.5 py-1 text-xs rounded {range === id ? 'bg-neutral-800 text-white' : 'text-neutral-400 hover:text-white'}"
        >{label}</button
      >
    {/each}
  </div>
  <span class="text-xs text-neutral-500">
    {range === "live" ? "Last 30 min, updates live" : "One-minute averages"}
  </span>
</div>

{#if error}<p class="text-sm text-red-400 mb-3">{error}</p>{/if}

<div class="grid gap-4 md:grid-cols-2">
  <TimeChart title={cpuTitle} {ts} series={[{ label: "CPU", values: samples.map((s) => s.cpu) }]} format={percent} yMax={appId ? undefined : 100} floor={1} />
  <TimeChart
    title={memLimit ? `Memory (of ${bytes(memLimit)})` : "Memory"}
    {ts}
    series={[{ label: "Used", values: samples.map((s) => s.memUsed) }]}
    format={(v) => bytes(v)}
    yMax={memLimit || undefined}
  />
  <TimeChart
    title="Network"
    {ts}
    series={[
      { label: "Received", values: samples.map((s) => s.netRx) },
      { label: "Sent", values: samples.map((s) => s.netTx) },
    ]}
    format={rate}
    floor={1024}
  />
  <TimeChart
    title="Disk I/O"
    {ts}
    series={[
      { label: "Read", values: samples.map((s) => s.blkRead) },
      { label: "Write", values: samples.map((s) => s.blkWrite) },
    ]}
    format={rate}
    floor={1024}
  />
</div>
