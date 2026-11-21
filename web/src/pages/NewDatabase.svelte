<script lang="ts">
  import { onMount } from "svelte";
  import { api, type EngineInfo } from "../lib/api";
  import { router } from "../lib/router.svelte";
  import Field from "../lib/Field.svelte";

  let { projectId }: { projectId: string } = $props();

  let engines = $state<EngineInfo[]>([]);
  let engine = $state("postgres");
  let version = $state("");
  let name = $state("");
  let memoryMb = $state("");
  let error = $state("");
  let busy = $state(false);

  const current = $derived(engines.find((e) => e.name === engine));

  onMount(async () => {
    engines = (await api<{ engines: EngineInfo[] }>("/databases/engines")).engines;
    version = engines[0]?.versions[0] ?? "";
  });

  function pick(e: EngineInfo) {
    engine = e.name;
    version = e.versions[0];
    if (!name) name = e.name === "redis" ? "cache" : "db";
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = "";
    busy = true;
    try {
      const res = await api<{ id: number }>(`/projects/${projectId}/databases`, {
        json: { name: name.trim(), engine, version, memoryMb: memoryMb ? Number(memoryMb) : 0 },
      });
      router.go(`/databases/${res.id}`);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<h1 class="text-xl font-semibold mb-6">New database</h1>

<form onsubmit={submit} class="max-w-2xl space-y-5">
  <div class="grid grid-cols-2 md:grid-cols-4 gap-3" role="radiogroup" aria-label="Engine">
    {#each engines as e}
      <button
        type="button"
        role="radio"
        aria-checked={engine === e.name}
        onclick={() => pick(e)}
        class="rounded-lg border p-3 text-left {engine === e.name ? 'border-emerald-500 bg-emerald-950/30' : 'border-neutral-800 hover:border-neutral-600'}"
      >
        <span class="block font-medium">{e.label}</span>
        <span class="block text-xs text-neutral-500 mt-0.5">port {e.port}</span>
      </button>
    {/each}
  </div>

  <div class="grid gap-4 md:grid-cols-3">
    <Field label="Name" bind:value={name} hint="Also its hostname inside the project" />
    <label class="block">
      <span class="block text-sm text-neutral-300 mb-1">Version</span>
      <select bind:value={version} class="w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm">
        {#each current?.versions ?? [] as v}<option value={v}>{v}</option>{/each}
      </select>
    </label>
    <Field label="Memory limit (MB)" bind:value={memoryMb} inputmode="numeric" hint="Empty = no limit" />
  </div>

  <p class="text-sm text-neutral-400">
    A strong password is generated and stored encrypted. Data lives in its own Docker volume. The database is reachable
    only by apps in this project until you enable public access. Daily backups (keep 7) are on by default.
  </p>

  {#if error}<p class="text-sm text-red-400">{error}</p>{/if}
  <button disabled={busy || !current} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-4 py-2 text-sm font-medium">
    {busy ? "Creating…" : `Create ${current?.label ?? "database"}`}
  </button>
</form>
