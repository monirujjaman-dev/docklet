<script lang="ts">
  import { api, apiDownload, type UpdateInfo } from "./api";

  // --- Backup ---
  let password = $state("");
  let passphrase = $state("");
  let confirmPass = $state("");
  let withDBs = $state(true);
  let busy = $state(false);
  let msg = $state("");
  let error = $state("");

  async function download(e: SubmitEvent) {
    e.preventDefault();
    error = msg = "";
    if (passphrase.length < 12) {
      error = "Use a passphrase of at least 12 characters.";
      return;
    }
    if (passphrase !== confirmPass) {
      error = "The passphrases don't match.";
      return;
    }
    busy = true;
    try {
      const name = await apiDownload("/system/backup", { password, passphrase, includeDatabaseBackups: withDBs });
      msg = `Downloaded ${name}. Store it off this server, and keep the passphrase: without it the backup can't be opened.`;
      password = passphrase = confirmPass = "";
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }

  // --- Updates ---
  let upd = $state<UpdateInfo | null>(null);
  let checking = $state(false);
  async function checkUpdates(refresh: boolean) {
    checking = true;
    try {
      upd = await api<UpdateInfo>(`/system/update${refresh ? "?refresh=1" : ""}`);
    } catch (err) {
      upd = { current: "", newer: false, comparable: false, inContainer: false, checkedAt: 0, error: String(err) };
    } finally {
      checking = false;
    }
  }
  const installCmd = "curl -sSL https://raw.githubusercontent.com/monirujjaman-dev/docklet/main/install.sh | sudo sh";
  const input = "w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm outline-none focus:border-emerald-500";
</script>

<section class="max-w-2xl space-y-4 mt-12 pt-8 border-t border-neutral-800">
  <div>
    <h2 class="font-medium">Back up Docklet</h2>
    <p class="text-sm text-neutral-400 mt-1">
      Downloads Docklet's database (projects, apps, domains, settings, audit log) and its master key, encrypted with a
      passphrase you choose. Restore it with <span class="font-mono">docklet restore</span>, see
      <a class="text-emerald-300 hover:underline" href="https://github.com/monirujjaman-dev/docklet/blob/main/docs/backup.md" target="_blank" rel="noreferrer">docs/backup.md</a>.
      App images and volumes are not included.
    </p>
  </div>
  <form onsubmit={download} class="grid gap-3 md:grid-cols-2">
    <label class="text-sm md:col-span-2">Your password
      <input type="password" bind:value={password} autocomplete="current-password" class="mt-1 {input}" /></label>
    <label class="text-sm">Encryption passphrase
      <input type="password" bind:value={passphrase} autocomplete="new-password" class="mt-1 {input}" /></label>
    <label class="text-sm">Repeat passphrase
      <input type="password" bind:value={confirmPass} autocomplete="new-password" class="mt-1 {input}" /></label>
    <label class="flex items-center gap-2 text-sm text-neutral-300 md:col-span-2">
      <input type="checkbox" bind:checked={withDBs} /> Include the newest backup of each managed database
    </label>
    <div class="md:col-span-2 flex items-center gap-3">
      <button disabled={busy || !password} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-4 py-2 text-sm font-medium">
        {busy ? "Preparing…" : "Download backup"}
      </button>
    </div>
    {#if error}<p class="text-sm text-red-400 md:col-span-2">{error}</p>{/if}
    {#if msg}<p class="text-sm text-emerald-300 md:col-span-2">{msg}</p>{/if}
  </form>
</section>

<section class="max-w-2xl space-y-3 mt-12 pt-8 border-t border-neutral-800">
  <h2 class="font-medium">Updates</h2>
  {#if !upd}
    <p class="text-sm text-neutral-400">Checks GitHub for the latest release. Nothing is sent about this server.</p>
    <button onclick={() => checkUpdates(false)} disabled={checking} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50">
      {checking ? "Checking…" : "Check for updates"}
    </button>
  {:else if upd.disabled}
    <p class="text-sm text-neutral-400">Update checks are disabled on this server (DOCKLET_UPDATE_CHECK=off). Running {upd.current}.</p>
  {:else}
    <p class="text-sm">
      Running <span class="font-mono">{upd.current}</span>
      {#if upd.latest}· latest release <a class="text-emerald-300 hover:underline font-mono" href={upd.url} target="_blank" rel="noreferrer">{upd.latest}</a>{/if}
    </p>
    {#if upd.error}<p class="text-sm text-red-400">{upd.error}</p>
    {:else if upd.newer}
      <p class="text-sm text-amber-300">An update is available. Your apps, databases and domains are kept when upgrading.</p>
    {:else if upd.comparable}
      <p class="text-sm text-emerald-300">Docklet is up to date.</p>
    {:else}
      <p class="text-sm text-neutral-400">This is a development build ({upd.current}), so it can't be compared with releases.</p>
    {/if}
    {#if upd.newer || !upd.comparable}
      <p class="text-xs text-neutral-500">{upd.inContainer ? "To upgrade, run on the server:" : "To upgrade this binary install, run:"}</p>
      <pre class="rounded-md bg-neutral-900 border border-neutral-800 p-2 text-xs font-mono overflow-x-auto select-all">{upd.inContainer ? installCmd : "sudo docklet upgrade -restart"}</pre>
    {/if}
    <button onclick={() => checkUpdates(true)} disabled={checking} class="text-xs text-neutral-400 hover:text-white">{checking ? "Checking…" : "Check again"}</button>
  {/if}
</section>
