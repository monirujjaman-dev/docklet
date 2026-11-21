<script lang="ts">
  import { reauth } from "./reauth.svelte";
  let pw = $state("");
  async function submit(e: SubmitEvent) {
    e.preventDefault();
    await reauth.submit(pw);
    if (!reauth.open) pw = "";
  }
</script>

{#if reauth.open}
  <div class="fixed inset-0 z-50 bg-black/60 flex items-center justify-center p-4">
    <form onsubmit={submit} class="w-full max-w-sm rounded-lg border border-neutral-700 bg-neutral-950 p-5 space-y-3">
      <h2 class="font-medium">Confirm it's you</h2>
      <p class="text-sm text-neutral-400">Enter your password to continue. You won't be asked again for 10 minutes.</p>
      <!-- svelte-ignore a11y_autofocus -->
      <input type="password" bind:value={pw} autocomplete="current-password" autofocus class="w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm outline-none focus:border-emerald-500" />
      {#if reauth.error}<p class="text-sm text-red-400">{reauth.error}</p>{/if}
      <div class="flex gap-2 justify-end">
        <button type="button" onclick={() => { pw = ""; reauth.cancel(); }} class="px-3 py-1.5 text-sm text-neutral-400">Cancel</button>
        <button disabled={!pw} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">Continue</button>
      </div>
    </form>
  </div>
{/if}
