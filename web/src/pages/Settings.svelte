<script lang="ts">
  import { onMount } from "svelte";
  import { api, type PanelState } from "../lib/api";
  import Field from "../lib/Field.svelte";
  import SystemPanel from "../lib/SystemPanel.svelte";

  let panel = $state<PanelState | null>(null);
  let domain = $state("");
  let acmeEmail = $state("");
  let error = $state("");
  let busy = $state(false);
  let timer: ReturnType<typeof setInterval> | undefined;

  async function load(syncForm = false) {
    panel = await api<PanelState>("/settings/panel");
    if (syncForm) {
      domain = panel.domain;
      acmeEmail = panel.acmeEmail;
    }
    // Poll while a verification is running.
    if (panel.status === "pending" && !timer) timer = setInterval(() => load(), 2000);
    if (panel.status !== "pending" && timer) {
      clearInterval(timer);
      timer = undefined;
    }
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    error = "";
    busy = true;
    try {
      panel = await api<PanelState>("/settings/panel", { method: "PUT", json: { domain, acmeEmail } });
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }

  async function remove() {
    if (!confirm("Remove the panel domain? Direct access on the server's port will be re-enabled.")) return;
    error = "";
    try {
      panel = await api<PanelState>("/settings/panel", { method: "DELETE" });
      domain = "";
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  const badge: Record<PanelState["status"], [string, string]> = {
    none: ["Not configured", "bg-neutral-800 text-neutral-300"],
    pending: ["Verifying…", "bg-amber-950 text-amber-300"],
    active: ["Active", "bg-emerald-950 text-emerald-300"],
    failed: ["Verification failed", "bg-red-950 text-red-300"],
  };

  onMount(() => {
    load(true);
    return () => timer && clearInterval(timer);
  });
</script>

<h1 class="text-xl font-semibold mb-6">Settings</h1>

<section class="max-w-2xl space-y-5">
  <div class="flex items-center justify-between">
    <h2 class="font-medium">Panel domain</h2>
    {#if panel}
      <span class="rounded-full px-2.5 py-0.5 text-xs {badge[panel.status][1]}">{badge[panel.status][0]}</span>
    {/if}
  </div>

  <p class="text-sm text-neutral-400">
    Serve this dashboard at <code class="text-neutral-300">https://your-domain</code> with an automatic certificate. Point
    the domain's DNS A/AAAA record to this server and keep ports 80 and 443 open. Once HTTPS works, direct access on
    port {panel?.listen.split(":").pop() ?? "3000"} is limited to localhost.
  </p>

  {#if panel?.caddy && !panel.caddy.reachable}
    <div class="rounded-md border border-red-900 bg-red-950/40 p-3 text-sm text-red-300">
      Caddy's admin API is not reachable{panel.caddy.lastError ? `: ${panel.caddy.lastError}` : ""}. Is Caddy running?
    </div>
  {:else if panel && !panel.caddy}
    <div class="rounded-md border border-neutral-800 p-3 text-sm text-neutral-400">
      Caddy integration is disabled (<code>-caddy-admin off</code>).
    </div>
  {/if}

  <form onsubmit={save} class="space-y-3">
    <Field label="Domain" bind:value={domain} hint="e.g. panel.example.com" />
    <Field
      label="ACME e-mail (optional)"
      type="email"
      bind:value={acmeEmail}
      hint="Let's Encrypt uses it for expiry and account notices"
    />
    {#if error}<p class="text-sm text-red-400">{error}</p>{/if}
    <div class="flex gap-3">
      <button
        disabled={busy || !panel?.caddy}
        class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-2 text-sm font-medium"
      >
        {busy ? "Saving…" : "Save and verify"}
      </button>
      {#if panel && panel.status !== "none"}
        <button type="button" onclick={remove} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-2 text-sm">
          Remove domain
        </button>
      {/if}
    </div>
  </form>

  {#if panel?.message}
    <p class="text-sm {panel.status === 'failed' ? 'text-red-300' : 'text-neutral-400'}">{panel.message}</p>
  {/if}
  {#if panel?.status === "active"}
    <p class="text-sm text-emerald-300">
      Dashboard is live at <a class="underline" href="https://{panel.domain}">https://{panel.domain}</a>.
    </p>
  {/if}

  {#if panel}
    <dl class="text-xs text-neutral-500 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 pt-2">
      <dt>Listening on</dt>
      <dd class="font-mono">{panel.listen}</dd>
      {#if panel.caddy?.lastSync}
        <dt>Last Caddy sync</dt>
        <dd>{new Date(panel.caddy.lastSync).toLocaleString()}</dd>
      {/if}
    </dl>
  {/if}
</section>

<SystemPanel />
