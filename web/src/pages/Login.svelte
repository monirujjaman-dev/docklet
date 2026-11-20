<script lang="ts">
  import { api, ApiError, type Me } from "../lib/api";
  import { session } from "../lib/session.svelte";
  import Field from "../lib/Field.svelte";

  let username = $state("");
  let password = $state("");
  let totp = $state("");
  let needTotp = $state(false);
  let error = $state("");
  let busy = $state(false);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = "";
    busy = true;
    try {
      const body: Record<string, string> = { username, password };
      if (needTotp) body.totp = totp.trim();
      session.apply(await api<Me>("/auth/login", { json: body }));
    } catch (err) {
      if (err instanceof ApiError && err.body.totpRequired) {
        if (needTotp) error = err.message;
        needTotp = true;
      } else {
        error = err instanceof Error ? err.message : String(err);
      }
    } finally {
      busy = false;
    }
  }
</script>

<div class="min-h-screen grid place-items-center p-6">
  <form onsubmit={submit} class="w-full max-w-sm space-y-4">
    <h1 class="text-xl font-semibold">Sign in to Docklet</h1>
    {#if !needTotp}
      <Field label="Username" bind:value={username} autocomplete="username" />
      <Field label="Password" type="password" bind:value={password} autocomplete="current-password" />
    {:else}
      <Field
        label="Two-factor code"
        bind:value={totp}
        autocomplete="one-time-code"
        inputmode="numeric"
        hint="6-digit code from your authenticator app"
      />
    {/if}
    {#if error}<p class="text-sm text-red-400">{error}</p>{/if}
    <button
      disabled={busy}
      class="w-full rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 py-2 text-sm font-medium"
    >
      {busy ? "Signing in…" : needTotp ? "Verify" : "Sign in"}
    </button>
  </form>
</div>
