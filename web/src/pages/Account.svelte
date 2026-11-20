<script lang="ts">
  import { api } from "../lib/api";
  import { session } from "../lib/session.svelte";
  import Field from "../lib/Field.svelte";

  // Change password
  let currentPassword = $state("");
  let newPassword = $state("");
  let pwMsg = $state<{ ok: boolean; text: string } | null>(null);

  async function changePassword(e: SubmitEvent) {
    e.preventDefault();
    pwMsg = null;
    try {
      await api("/account/password", { json: { currentPassword, newPassword } });
      pwMsg = { ok: true, text: "Password changed. Other sessions were signed out." };
      currentPassword = newPassword = "";
    } catch (err) {
      pwMsg = { ok: false, text: err instanceof Error ? err.message : String(err) };
    }
  }

  // Two-factor
  let pending = $state<{ secret: string; uri: string } | null>(null);
  let code = $state("");
  let disablePw = $state("");
  let tfMsg = $state<{ ok: boolean; text: string } | null>(null);

  async function startTotp() {
    tfMsg = null;
    pending = await api<{ secret: string; uri: string }>("/account/totp/setup", { json: {} });
  }

  async function enableTotp(e: SubmitEvent) {
    e.preventDefault();
    tfMsg = null;
    try {
      await api("/account/totp/enable", { json: { code: code.trim() } });
      pending = null;
      code = "";
      await session.refresh();
      tfMsg = { ok: true, text: "Two-factor authentication enabled." };
    } catch (err) {
      tfMsg = { ok: false, text: err instanceof Error ? err.message : String(err) };
    }
  }

  async function disableTotp(e: SubmitEvent) {
    e.preventDefault();
    tfMsg = null;
    try {
      await api("/account/totp/disable", { json: { password: disablePw } });
      disablePw = "";
      await session.refresh();
      tfMsg = { ok: true, text: "Two-factor authentication disabled." };
    } catch (err) {
      tfMsg = { ok: false, text: err instanceof Error ? err.message : String(err) };
    }
  }
</script>

<h1 class="text-xl font-semibold mb-6">Account</h1>

<div class="grid gap-8 md:grid-cols-2 max-w-4xl">
  <section class="space-y-4">
    <h2 class="font-medium">Change password</h2>
    <form onsubmit={changePassword} class="space-y-3">
      <Field label="Current password" type="password" bind:value={currentPassword} autocomplete="current-password" />
      <Field label="New password" type="password" bind:value={newPassword} autocomplete="new-password" hint="At least 10 characters" />
      {#if pwMsg}<p class="text-sm {pwMsg.ok ? 'text-emerald-400' : 'text-red-400'}">{pwMsg.text}</p>{/if}
      <button class="rounded-md bg-emerald-600 hover:bg-emerald-500 px-3 py-2 text-sm font-medium">Update password</button>
    </form>
  </section>

  <section class="space-y-4">
    <h2 class="font-medium">Two-factor authentication</h2>
    {#if session.user?.totpEnabled}
      <p class="text-sm text-emerald-400">Enabled</p>
      <form onsubmit={disableTotp} class="space-y-3">
        <Field label="Confirm with your password to disable" type="password" bind:value={disablePw} autocomplete="current-password" />
        <button class="rounded-md border border-red-700 text-red-300 hover:bg-red-950 px-3 py-2 text-sm">Disable 2FA</button>
      </form>
    {:else if pending}
      <p class="text-sm text-neutral-400">
        Add this key to your authenticator app (Google Authenticator, 1Password, Aegis…), then enter the 6-digit code.
      </p>
      <div class="rounded-md bg-neutral-900 border border-neutral-800 p-3 space-y-2">
        <p class="text-xs text-neutral-500">Setup key</p>
        <p class="font-mono text-sm break-all select-all">{pending.secret}</p>
        <p class="text-xs text-neutral-500 pt-1">otpauth URI</p>
        <p class="font-mono text-xs break-all select-all text-neutral-400">{pending.uri}</p>
      </div>
      <form onsubmit={enableTotp} class="space-y-3">
        <Field label="Code" bind:value={code} autocomplete="one-time-code" inputmode="numeric" />
        <button class="rounded-md bg-emerald-600 hover:bg-emerald-500 px-3 py-2 text-sm font-medium">Enable 2FA</button>
      </form>
    {:else}
      <p class="text-sm text-neutral-400">Not enabled. Adds a 6-digit code from an authenticator app to every sign-in.</p>
      <button onclick={startTotp} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-2 text-sm">
        Set up 2FA
      </button>
    {/if}
    {#if tfMsg}<p class="text-sm {tfMsg.ok ? 'text-emerald-400' : 'text-red-400'}">{tfMsg.text}</p>{/if}
  </section>
</div>
