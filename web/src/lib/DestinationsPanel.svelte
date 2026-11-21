<script lang="ts">
  import { onMount } from "svelte";
  import { api, type Destination } from "./api";

  let list = $state<Destination[]>([]);
  let error = $state("");
  let editing = $state<number | "new" | null>(null);
  let busy = $state(false);
  let testResult = $state<{ ok: boolean; error?: string } | null>(null);

  const empty = { name: "", endpoint: "", region: "", bucket: "", prefix: "docklet", accessKey: "", secretKey: "", pathStyle: false };
  let form = $state({ ...empty });

  const presets: { label: string; endpoint: string; region: string; pathStyle: boolean; hint: string }[] = [
    { label: "Amazon S3", endpoint: "https://s3.eu-central-1.amazonaws.com", region: "eu-central-1", pathStyle: false, hint: "Use your bucket's region in both fields." },
    { label: "Cloudflare R2", endpoint: "https://<account-id>.r2.cloudflarestorage.com", region: "auto", pathStyle: true, hint: "Account ID is on the R2 overview page." },
    { label: "Backblaze B2", endpoint: "https://s3.us-west-004.backblazeb2.com", region: "us-west-004", pathStyle: false, hint: "Copy the endpoint shown on the bucket page." },
    { label: "Wasabi", endpoint: "https://s3.eu-central-1.wasabisys.com", region: "eu-central-1", pathStyle: false, hint: "" },
    { label: "DigitalOcean Spaces", endpoint: "https://fra1.digitaloceanspaces.com", region: "fra1", pathStyle: false, hint: "" },
    { label: "MinIO / Garage / other", endpoint: "https://s3.example.com", region: "us-east-1", pathStyle: true, hint: "Self-hosted services usually need path-style." },
  ];
  let hint = $state("");
  function preset(i: number) {
    const p = presets[i];
    form.endpoint = p.endpoint;
    form.region = p.region;
    form.pathStyle = p.pathStyle;
    hint = p.hint;
  }

  async function load() {
    try {
      list = (await api<{ destinations: Destination[] }>("/destinations")).destinations;
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  function edit(d: Destination | null) {
    testResult = null;
    error = "";
    hint = "";
    if (d) {
      editing = d.id;
      form = { name: d.name, endpoint: d.endpoint, region: d.region, bucket: d.bucket, prefix: d.prefix, accessKey: d.accessKey, secretKey: "", pathStyle: d.pathStyle };
    } else {
      editing = "new";
      form = { ...empty };
    }
  }

  async function run(fn: () => Promise<unknown>) {
    error = "";
    busy = true;
    try {
      await fn();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }

  const test = () =>
    run(async () => {
      if (editing !== "new" && !form.secretKey) {
        // Stored secret: test the saved destination after saving.
        testResult = { ok: false, error: "Enter the secret key to test unsaved changes, or save and use Test in the list." };
        return;
      }
      testResult = await api("/destinations/test", { json: form });
    });

  const save = (e: SubmitEvent) => {
    e.preventDefault();
    run(async () => {
      const d =
        editing === "new"
          ? await api<Destination>("/destinations", { json: form })
          : await api<Destination>(`/destinations/${editing}`, { method: "PUT", json: form });
      editing = null;
      await load();
      if (d.lastTestError) error = `Saved, but the connection test failed: ${d.lastTestError}`;
    });
  };

  const testSaved = (d: Destination) => run(async () => { await api(`/destinations/${d.id}/test`, { method: "POST" }); await load(); });
  const remove = (d: Destination) => {
    if (!confirm(`Delete destination "${d.name}"? Files already in the bucket are kept.`)) return;
    run(async () => { await api(`/destinations/${d.id}`, { method: "DELETE" }); await load(); });
  };

  onMount(load);
  const input = "mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm outline-none focus:border-emerald-500";
  const fmt = (ts?: number) => (ts ? new Date(ts * 1000).toLocaleString() : "");
</script>

<section class="max-w-3xl space-y-4 mt-12 pt-8 border-t border-neutral-800">
  <div class="flex items-center justify-between gap-4">
    <div>
      <h2 class="font-medium">Backup destinations (S3)</h2>
      <p class="text-sm text-neutral-400 mt-1">Any S3-compatible bucket: AWS, Cloudflare R2, Backblaze B2, Wasabi, Spaces, MinIO. Choose one per database under Backups, and for Docklet's own backups below.</p>
    </div>
    {#if editing === null}
      <button onclick={() => edit(null)} class="shrink-0 rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Add destination</button>
    {/if}
  </div>
  {#if error}<p class="text-sm text-red-400">{error}</p>{/if}

  {#if editing !== null}
    <form onsubmit={save} class="rounded-lg border border-neutral-800 p-4 space-y-3">
      <div class="flex flex-wrap gap-1.5">
        {#each presets as p, i}
          <button type="button" onclick={() => preset(i)} class="rounded-full border border-neutral-800 hover:border-neutral-600 px-2.5 py-0.5 text-xs text-neutral-300">{p.label}</button>
        {/each}
      </div>
      {#if hint}<p class="text-xs text-neutral-500">{hint}</p>{/if}
      <div class="grid gap-3 md:grid-cols-2">
        <label class="text-sm">Name<input bind:value={form.name} placeholder="offsite" class={input} /></label>
        <label class="text-sm">Bucket<input bind:value={form.bucket} class={input} /></label>
        <label class="text-sm md:col-span-2">Endpoint<input bind:value={form.endpoint} placeholder="https://s3.eu-central-1.amazonaws.com" class="{input} font-mono" /></label>
        <label class="text-sm">Region<input bind:value={form.region} placeholder="us-east-1" class={input} /></label>
        <label class="text-sm">Folder (prefix)<input bind:value={form.prefix} placeholder="docklet" class={input} /></label>
        <label class="text-sm">Access key<input bind:value={form.accessKey} autocomplete="off" class="{input} font-mono" /></label>
        <label class="text-sm">Secret key
          <input type="password" bind:value={form.secretKey} autocomplete="new-password" placeholder={editing === "new" ? "" : "unchanged"} class="{input} font-mono" /></label>
      </div>
      <label class="flex items-center gap-2 text-sm text-neutral-300"><input type="checkbox" bind:checked={form.pathStyle} /> Path-style URLs (endpoint/bucket/key)</label>
      <p class="text-xs text-neutral-500">Use a key that can only access this bucket. The secret is stored encrypted.</p>
      {#if testResult}
        <p class="text-sm {testResult.ok ? 'text-emerald-300' : 'text-red-400'}">{testResult.ok ? "Connection works: wrote, read and deleted a test file." : testResult.error}</p>
      {/if}
      <div class="flex gap-2">
        <button disabled={busy} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">Save</button>
        <button type="button" onclick={test} disabled={busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50">{busy ? "Testing…" : "Test connection"}</button>
        <button type="button" onclick={() => (editing = null)} class="px-3 py-1.5 text-sm text-neutral-400 hover:text-white">Cancel</button>
      </div>
    </form>
  {/if}

  <div class="rounded-md border border-neutral-800 divide-y divide-neutral-800">
    {#each list as d (d.id)}
      <div class="flex items-center justify-between gap-4 px-3 py-2 text-sm">
        <div class="min-w-0">
          <span class="font-medium">{d.name}</span>
          <span class="text-xs text-neutral-500 font-mono ml-2 truncate">{d.bucket}{d.prefix ? "/" + d.prefix : ""} · {d.endpoint}</span>
          <div class="text-xs mt-0.5 {d.lastTestError ? 'text-red-400' : 'text-neutral-500'}">
            {#if d.lastTestError}Last test failed: {d.lastTestError}{:else if d.lastTestAt}Last test OK · {fmt(d.lastTestAt)}{/if}
            {#if d.usedBy}· used by {d.usedBy}{/if}
          </div>
        </div>
        <span class="shrink-0 space-x-3">
          <button onclick={() => testSaved(d)} disabled={busy} class="text-xs text-neutral-300 hover:underline">Test</button>
          <button onclick={() => edit(d)} class="text-xs text-neutral-300 hover:underline">Edit</button>
          <button onclick={() => remove(d)} class="text-xs text-neutral-500 hover:text-red-300">Delete</button>
        </span>
      </div>
    {:else}
      <p class="px-3 py-4 text-sm text-neutral-500">No destinations yet.</p>
    {/each}
  </div>
</section>
