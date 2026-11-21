<script lang="ts">
  import { onDestroy } from "svelte";
  import { api, type UpdateInfo } from "./api";

  let { upd }: { upd: UpdateInfo } = $props();

  type Step = { at: string; msg: string };
  type Status = { id: string; state: "running" | "succeeded" | "rolled_back" | "failed"; from: string; to: string; image: string; startedAt: string; finishedAt?: string; error?: string; backup?: string; steps: Step[]; auto?: boolean };
  type StatusResp = { current: string; inContainer: boolean; last: Status | null; auto: { patch: boolean; hourUtc: number } };

  let st = $state<StatusResp | null>(null);
  let pw = $state("");
  let version = $state("");
  let error = $state("");
  let down = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;

  async function poll() {
    clearTimeout(timer);
    try {
      st = await api<StatusResp>("/system/update/status");
      down = false;
    } catch {
      down = true; // Docklet is restarting
    }
    if (down || st?.last?.state === "running") timer = setTimeout(poll, 2000);
  }
  poll();
  onDestroy(() => clearTimeout(timer));

  async function start() {
    error = "";
    try {
      await api("/system/update/start", { json: { version: version.trim(), password: pw } });
      pw = "";
      setTimeout(poll, 500);
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }
  async function saveAuto(patch: boolean, hourUtc: number) {
    error = "";
    try {
      await api("/system/update/auto", { method: "PUT", json: { patch, hourUtc } });
      await poll();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }
  const running = $derived(st?.last?.state === "running" || down);
  const labels = { running: "Updating…", succeeded: "Updated", rolled_back: "Rolled back", failed: "Failed" } as const;
  const input = "rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm outline-none focus:border-emerald-500";
</script>

{#if upd.inContainer}
  <div class="space-y-3 rounded-lg border border-neutral-800 p-4">
    {#if !running}
      <div class="flex flex-wrap gap-2 items-end">
        <label class="text-sm">Version<input bind:value={version} placeholder={upd.latest ? `${upd.latest.replace(/^v/, "")} (latest)` : "latest"} class="mt-1 block w-44 {input} font-mono" /></label>
        <label class="text-sm">Your password<input type="password" bind:value={pw} autocomplete="current-password" class="mt-1 block {input}" /></label>
        <button onclick={start} disabled={!pw} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">Update now</button>
      </div>
      <p class="text-xs text-neutral-500">Docklet backs up its database, pulls the new image and restarts in a few seconds. Apps, databases and Caddy keep running. If the new version doesn't come up healthy within 2 minutes, the previous version is started again with its database.</p>
    {/if}
    {#if down}<p class="text-sm text-amber-300">Docklet is restarting…</p>{/if}
    {#if st?.last}
      <div class="text-sm space-y-1">
        <p>
          <span class="rounded px-2 py-0.5 text-xs {st.last.state === 'succeeded' ? 'bg-emerald-900 text-emerald-200' : st.last.state === 'running' ? 'bg-sky-900 text-sky-200' : 'bg-red-950 text-red-300'}">{labels[st.last.state]}</span>
          <span class="font-mono ml-1">{st.last.from} → {st.last.to}</span>
          <span class="text-neutral-500 text-xs ml-1">{new Date(st.last.startedAt).toLocaleString()}{st.last.auto ? " · automatic" : ""}</span>
        </p>
        {#if st.last.error}<p class="text-red-400">{st.last.error}</p>{/if}
        <ol class="text-xs font-mono text-neutral-400 max-h-60 overflow-y-auto space-y-0.5 whitespace-pre-wrap">
          {#each st.last.steps as s}<li><span class="text-neutral-600">{new Date(s.at).toLocaleTimeString()}</span> {s.msg}</li>{/each}
        </ol>
      </div>
    {/if}
    {#if st}
      <label class="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={st.auto.patch} onchange={(e) => saveAuto(e.currentTarget.checked, st!.auto.hourUtc)} />
        Install patch releases automatically (e.g. {st.current.replace(/\.\d+$/, ".x")}) between
        <select value={st.auto.hourUtc} onchange={(e) => saveAuto(st!.auto.patch, Number(e.currentTarget.value))} class="{input} py-0.5">
          {#each Array.from({ length: 24 }, (_, i) => i) as h}<option value={h}>{String(h).padStart(2, "0")}:00</option>{/each}
        </select> and the next hour (UTC)
      </label>
    {/if}
    {#if error}<p class="text-sm text-red-400">{error}</p>{/if}
  </div>
{/if}
