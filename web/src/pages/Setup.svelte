<script lang="ts">
  import { api, type Me } from "../lib/api";
  import { session } from "../lib/session.svelte";
  import Field from "../lib/Field.svelte";

  let username = $state("admin");
  let password = $state("");
  let confirm = $state("");
  let error = $state("");
  let busy = $state(false);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = "";
    if (password !== confirm) {
      error = "Passwords do not match";
      return;
    }
    busy = true;
    try {
      session.apply(await api<Me>("/setup", { json: { username, password } }));
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="min-h-screen grid place-items-center p-6">
  <form onsubmit={submit} class="w-full max-w-sm space-y-4">
    <div>
      <h1 class="text-xl font-semibold">Welcome to Docklet</h1>
      <p class="text-sm text-neutral-400 mt-1">Create the administrator account. This page is only shown once.</p>
    </div>
    <Field label="Username" bind:value={username} autocomplete="username" />
    <Field label="Password" type="password" bind:value={password} autocomplete="new-password" hint="At least 10 characters" />
    <Field label="Confirm password" type="password" bind:value={confirm} autocomplete="new-password" />
    {#if error}<p class="text-sm text-red-400">{error}</p>{/if}
    <button
      disabled={busy}
      class="w-full rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 py-2 text-sm font-medium"
    >
      {busy ? "Creating…" : "Create admin account"}
    </button>
  </form>
</div>
