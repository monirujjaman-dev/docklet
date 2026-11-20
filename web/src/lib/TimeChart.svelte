<script lang="ts">
  import { onMount } from "svelte";
  import uPlot from "uplot";
  import "uplot/dist/uPlot.min.css";

  // Categorical slots 1-2 of the reference palette, dark-mode steps,
  // validated against the dashboard surface (#0a0a0a).
  const SERIES = ["#3987e5", "#d95926"];

  let {
    title,
    ts,
    series,
    format,
    yMax,
    floor = 1,
    height = 160,
  }: {
    title: string;
    ts: number[]; // unix ms
    series: { label: string; values: number[] }[];
    format: (v: number) => string;
    yMax?: number;
    /** Smallest y-axis ceiling, so near-zero data doesn't produce noisy ticks. */
    floor?: number;
    height?: number;
  } = $props();

  let hoverIdx = $state<number | null>(null);
  const timeFmt = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" });

  let el: HTMLDivElement | undefined = $state();
  let plot: uPlot | null = null;
  const multi = $derived(series.length > 1);
  const latest = $derived(series.map((s) => s.values[s.values.length - 1]));
  // Header readout: the hovered point, otherwise the latest value.
  const readout = $derived(
    hoverIdx != null && ts[hoverIdx] !== undefined
      ? { value: series[0]?.values[hoverIdx], at: timeFmt.format(ts[hoverIdx]) }
      : { value: latest[0], at: "" },
  );

  function opts(width: number): uPlot.Options {
    return {
      width,
      height,
      padding: [8, 20, 0, 0],
      legend: { show: multi, live: true },
      cursor: { points: { size: 8 }, y: false, drag: { x: false, y: false } },
      scales: { x: { time: true }, y: { range: (_u, _min, max) => [0, yMax ?? Math.max(max * 1.1, floor)] } },
      hooks: {
        setCursor: [(u) => (hoverIdx = u.cursor.idx ?? null)],
      },
      axes: [
        { stroke: "#a3a3a3", grid: { stroke: "#262626", width: 1 }, ticks: { show: false }, font: "11px system-ui" },
        {
          stroke: "#a3a3a3",
          grid: { stroke: "#262626", width: 1 },
          ticks: { show: false },
          font: "11px system-ui",
          size: 64,
          values: (_u, vals) => vals.map((v) => format(v)),
        },
      ],
      series: [
        {},
        ...series.map((s, i) => ({
          label: s.label,
          stroke: SERIES[i],
          width: 2,
          fill: series.length === 1 ? SERIES[i] + "22" : undefined,
          points: { show: false },
          value: (_u: uPlot, v: number | null) => (v == null ? "–" : format(v)),
        })),
      ],
    };
  }

  function data(): uPlot.AlignedData {
    return [ts.map((t) => t / 1000), ...series.map((s) => s.values)];
  }

  onMount(() => {
    if (!el) return;
    plot = new uPlot(opts(el.clientWidth), data(), el);
    const ro = new ResizeObserver(() => el && plot?.setSize({ width: el.clientWidth, height }));
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot?.destroy();
      plot = null;
    };
  });

  $effect(() => {
    const d = data(); // track ts/series
    plot?.setData(d);
  });
</script>

<figure class="rounded-lg border border-neutral-800 p-3 min-w-0">
  <figcaption class="flex items-baseline justify-between mb-1">
    <span class="text-sm font-medium text-neutral-200">{title}</span>
    <span class="text-sm text-neutral-300 tabular-nums">
      {#if !multi && readout.value !== undefined}
        {#if readout.at}<span class="text-xs text-neutral-500 mr-2">{readout.at}</span>{/if}{format(readout.value)}
      {/if}
    </span>
  </figcaption>
  <div bind:this={el} class="w-full" role="img" aria-label="{title} over time"></div>
  {#if ts.length < 2}
    <p class="text-xs text-neutral-500 -mt-24 mb-20 text-center">Collecting data…</p>
  {/if}
</figure>

<style>
  :global(.uplot) {
    font-family: inherit;
  }
  :global(.u-legend) {
    color: #d4d4d4;
    font-size: 12px;
    text-align: left;
  }
  :global(.u-legend .u-marker) {
    border-radius: 2px;
  }
  :global(.u-legend th) {
    font-weight: 400;
  }
</style>
