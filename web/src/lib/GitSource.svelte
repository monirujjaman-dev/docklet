<script lang="ts">
  import { api, type AppDetail, type GitConfig } from "./api";

  let { app, onchange }: { app: AppDetail; onchange: (a: AppDetail) => void } = $props();

  // svelte-ignore state_referenced_locally
  const initial: GitConfig = app.git!;
  let form = $state({ ...initial });
  let msg = $state<{ ok: boolean; text: string } | null>(null);
  let webhook = $state<{ path: string; secret: string } | null>(null);
  let copied = $state("");

  const hookURL = $derived(webhook ? location.origin + webhook.path : app.git?.webhookPath ? location.origin + app.git.webhookPath : "");

  async function save(e: SubmitEvent) {
    e.preventDefault();
    msg = null;
    try {
      onchange(await api<AppDetail>(`/apps/${app.id}/git`, { method: "PUT", json: form }));
      msg = { ok: true, text: "Saved. The next deployment uses these settings." };
    } catch (err) {
      msg = { ok: false, text: err instanceof Error ? err.message : String(err) };
    }
  }

  async function revealWebhook() {
    webhook = await api(`/apps/${app.id}/webhook`);
  }

  async function rotateWebhook() {
    if (!confirm("Create a new webhook URL and secret? The old ones stop working immediately.")) return;
    webhook = await api(`/apps/${app.id}/webhook/rotate`, { method: "POST" });
    onchange(await api<AppDetail>(`/apps/${app.id}`));
  }

  async function regenerateKey() {
    if (!confirm("Create a new deploy key? Replace the old key in your repository's settings, or deploys will fail.")) return;
    onchange(await api<AppDetail>(`/apps/${app.id}/deploy-key`, { method: "POST" }));
  }

  async function copy(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      copied = what;
      setTimeout(() => (copied = ""), 1500);
    } catch {
      /* clipboard unavailable (http); the value is selectable */
    }
  }

  const input = "mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm";
</script>

<div class="grid gap-8 lg:grid-cols-2 max-w-6xl">
  <section class="space-y-3">
    <h2 class="font-medium">Git source</h2>
    <form onsubmit={save} class="grid grid-cols-2 gap-3 text-sm">
      <label class="col-span-2">Repository URL <input bind:value={form.url} class="{input} font-mono" /></label>
      <label>Branch <input bind:value={form.branch} class={input} /></label>
      <label>Build context <input bind:value={form.context} class={input} /></label>
      <label class="col-span-2">Dockerfile <input bind:value={form.dockerfile} class={input} /></label>
      <label class="col-span-2 flex items-center gap-2 text-neutral-300">
        <input type="checkbox" bind:checked={form.autoDeploy} /> Deploy automatically on push to <code>{form.branch}</code>
      </label>
      <div class="col-span-2">
        <button class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5">Save source</button>
      </div>
    </form>
    {#if msg}<p class="text-sm {msg.ok ? 'text-emerald-300' : 'text-red-400'}">{msg.text}</p>{/if}
  </section>

  <div class="space-y-8">
    {#if app.git?.ssh}
      <section class="space-y-2">
        <h2 class="font-medium">Deploy key</h2>
        <p class="text-sm text-neutral-400">
          Add this public key to the repository (GitHub: <em>Settings → Deploy keys</em>, read-only) so Docklet can clone
          it.
        </p>
        <div class="rounded-md bg-neutral-900 border border-neutral-800 p-3 font-mono text-xs break-all select-all">{app.git.deployKey}</div>
        <div class="flex gap-2 text-sm">
          <button onclick={() => copy(app.git?.deployKey ?? "", "key")} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1">
            {copied === "key" ? "Copied" : "Copy"}
          </button>
          <button onclick={regenerateKey} class="text-neutral-500 hover:text-red-300 px-2">Regenerate</button>
        </div>
      </section>
    {/if}

    <section class="space-y-2">
      <h2 class="font-medium">Webhook</h2>
      <p class="text-sm text-neutral-400">
        Add a <strong>push</strong> webhook in your Git host with content type <code>application/json</code>. GitHub and
        Gitea sign payloads with the secret; GitLab sends it as the secret token.
      </p>
      <dl class="text-sm space-y-2">
        <dt class="text-neutral-500 text-xs">Payload URL</dt>
        <dd class="rounded-md bg-neutral-900 border border-neutral-800 p-2 font-mono text-xs break-all select-all">{hookURL}</dd>
        <dt class="text-neutral-500 text-xs">Secret</dt>
        <dd>
          {#if webhook}
            <span class="rounded-md bg-neutral-900 border border-neutral-800 p-2 font-mono text-xs break-all select-all block">{webhook.secret}</span>
          {:else}
            <button onclick={revealWebhook} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1 text-xs">Show secret</button>
          {/if}
        </dd>
      </dl>
      <div class="flex gap-2 text-sm">
        <button onclick={() => copy(hookURL, "url")} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1">
          {copied === "url" ? "Copied" : "Copy URL"}
        </button>
        <button onclick={rotateWebhook} class="text-neutral-500 hover:text-red-300 px-2">Rotate URL and secret</button>
      </div>
      {#if location.hostname === "localhost" || location.hostname === "127.0.0.1"}
        <p class="text-xs text-amber-300">This URL uses localhost; your Git host can only reach it once the panel has a public domain.</p>
      {/if}
    </section>
  </div>
</div>
