<script lang="ts">
  import { onMount } from "svelte";
  import { api, type AuditEntry, type AuditVerify } from "../lib/api";

  let entries = $state<AuditEntry[]>([]);
  let nextBefore = $state<number | null>(null);
  let loading = $state(false);
  let error = $state("");
  let verify = $state<AuditVerify | null>(null);
  let verifying = $state(false);

  async function load(before?: number) {
    loading = true;
    error = "";
    try {
      const q = before ? `?before=${before}` : "";
      const res = await api<{ entries: AuditEntry[]; nextBefore?: number }>(`/audit${q}`);
      entries = before ? [...entries, ...res.entries] : res.entries;
      nextBefore = res.nextBefore ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  async function runVerify() {
    verifying = true;
    try {
      verify = await api<AuditVerify>("/audit/verify");
    } finally {
      verifying = false;
    }
  }

  const fmt = (ts: number) => new Date(ts).toLocaleString();
  const detailText = (d: Record<string, unknown>) =>
    Object.keys(d).length ? Object.entries(d).map(([k, v]) => `${k}=${v}`).join(" ") : "";

  onMount(() => load());
</script>

<div class="flex items-center justify-between mb-4">
  <h1 class="text-xl font-semibold">Audit log</h1>
  <div class="flex items-center gap-3">
    {#if verify}
      <span class="text-sm {verify.ok ? 'text-emerald-400' : 'text-red-400'}">
        {verify.ok
          ? `Chain intact · ${verify.checked} entries`
          : `Broken at #${verify.brokenAt}: ${verify.reason}`}
      </span>
    {/if}
    <button
      onclick={runVerify}
      disabled={verifying}
      class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50"
    >
      {verifying ? "Verifying…" : "Verify integrity"}
    </button>
  </div>
</div>

{#if error}<p class="text-sm text-red-400 mb-3">{error}</p>{/if}

<div class="overflow-x-auto rounded-md border border-neutral-800">
  <table class="w-full text-sm">
    <thead class="bg-neutral-900 text-neutral-400 text-left">
      <tr>
        <th class="px-3 py-2 font-medium">#</th>
        <th class="px-3 py-2 font-medium">Time</th>
        <th class="px-3 py-2 font-medium">Actor</th>
        <th class="px-3 py-2 font-medium">Action</th>
        <th class="px-3 py-2 font-medium">Target</th>
        <th class="px-3 py-2 font-medium">IP</th>
        <th class="px-3 py-2 font-medium">Details</th>
      </tr>
    </thead>
    <tbody>
      {#each entries as e (e.id)}
        <tr class="border-t border-neutral-800">
          <td class="px-3 py-2 text-neutral-500 font-mono">{e.id}</td>
          <td class="px-3 py-2 whitespace-nowrap">{fmt(e.ts)}</td>
          <td class="px-3 py-2">{e.actor}</td>
          <td class="px-3 py-2 font-mono {e.action.includes('failed') || e.action.includes('locked') ? 'text-amber-400' : ''}">
            {e.action}
          </td>
          <td class="px-3 py-2 text-neutral-400">{e.target}</td>
          <td class="px-3 py-2 text-neutral-400 font-mono">{e.ip}</td>
          <td class="px-3 py-2 text-neutral-500 font-mono text-xs">{detailText(e.details)}</td>
        </tr>
      {:else}
        {#if !loading}
          <tr><td colspan="7" class="px-3 py-6 text-center text-neutral-500">No entries yet</td></tr>
        {/if}
      {/each}
    </tbody>
  </table>
</div>

{#if nextBefore}
  <button
    onclick={() => load(nextBefore ?? undefined)}
    disabled={loading}
    class="mt-3 rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50"
  >
    {loading ? "Loading…" : "Load older"}
  </button>
{/if}
