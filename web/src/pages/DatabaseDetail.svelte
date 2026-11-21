<script lang="ts">
  import { onMount } from "svelte";
  import { api, type Backup, type DatabaseDetail, type DbUser, type Destination } from "../lib/api";
  import { link, router } from "../lib/router.svelte";
  import { bytes } from "../lib/format";
  import StatusBadge from "../lib/StatusBadge.svelte";

  let { dbId }: { dbId: string } = $props();

  let d = $state<DatabaseDetail | null>(null);
  let tab = $state<"connection" | "apps" | "users" | "backups">("connection");
  let error = $state("");
  let busy = $state("");
  let creds = $state<{ password: string; rootPassword?: string; url: string } | null>(null);
  let copied = $state("");

  async function load() {
    try {
      d = await api<DatabaseDetail>(`/databases/${dbId}`);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function run(label: string, fn: () => Promise<unknown>) {
    error = "";
    busy = label;
    try {
      await fn();
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = "";
    }
  }

  const action = (name: string) => run(name, () => api(`/databases/${dbId}/${name}`, { method: "POST" }));

  async function copy(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      copied = what;
      setTimeout(() => (copied = ""), 1500);
    } catch {
      /* selectable fallback */
    }
  }

  // Public access
  let publicPort = $state("");
  function setPublic(enable: boolean) {
    const port = enable ? Number(publicPort) : 0;
    if (enable && !confirm(`Publish ${d?.name} on port ${port} of this server? Anyone who can reach that port can try to log in.`)) return;
    run("public", () => api(`/databases/${dbId}/public`, { method: "PUT", json: { port } }));
  }
  let allow = $state("");
  const saveAllow = () => run("allow", () => api(`/databases/${dbId}/allowlist`, { method: "PUT", json: { sources: allow } }));

  // Links
  let linkApp = $state("");
  let linkKey = $state("");
  const linkable = $derived(d ? d.apps.filter((a) => !d!.links.some((l) => l.appId === a.id)) : []);
  function addLink(e: SubmitEvent) {
    e.preventDefault();
    run("link", () => api(`/databases/${dbId}/links`, { json: { appId: Number(linkApp), envKey: linkKey.trim() || d?.defaultEnvKey } }));
    linkApp = "";
  }

  // Users
  let newUser = $state("");
  let newRole = $state("readonly");
  let shown = $state<Record<number, { password: string; url: string }>>({});
  const roleText: Record<string, string> = {
    readonly: "Read only: SELECT / GET",
    readwrite: "Read & write data, no schema changes",
    admin: "All data, plus creating tables (PostgreSQL: may alter/drop only tables it created)",
  };
  function addUser(e: SubmitEvent) {
    e.preventDefault();
    run("user", async () => {
      const u = await api<DbUser>(`/databases/${dbId}/users`, { json: { username: newUser.trim(), role: newRole } });
      newUser = "";
      shown[u.id] = await api(`/database-users/${u.id}/credentials`);
    });
  }
  const reveal = (u: DbUser) => run("reveal", async () => { shown[u.id] = await api(`/database-users/${u.id}/credentials`); });
  const setRole = (u: DbUser, role: string) => run("role", () => api(`/database-users/${u.id}`, { method: "PATCH", json: { role } }));
  function resetPw(u: DbUser) {
    if (!confirm(`Generate a new password for ${u.username}? Apps using the old one stop connecting.`)) return;
    run("reset", async () => {
      await api(`/database-users/${u.id}`, { method: "PATCH", json: { resetPassword: true } });
      shown[u.id] = await api(`/database-users/${u.id}/credentials`);
    });
  }
  function removeUser(u: DbUser) {
    if (!confirm(`Delete user ${u.username}? Objects it owns are handed to ${d?.username ?? "the owner"}.`)) return;
    run("deluser", () => api(`/database-users/${u.id}`, { method: "DELETE" }));
  }

  // Backups
  let dests = $state<Destination[]>([]);
  let destSel = $state("");
  const setDest = () =>
    run("dest", () => api(`/databases/${dbId}/backup-destination`, { method: "PUT", json: { destinationId: destSel ? Number(destSel) : null } }));
  const retryUpload = (b: Backup) => run("upload", () => api(`/backups/${b.id}/upload`, { method: "POST" }));
  let schedule = $state("daily");
  let keep = $state("7");
  function savePolicy(e: SubmitEvent) {
    e.preventDefault();
    run("policy", () => api(`/databases/${dbId}/backup-policy`, { method: "PUT", json: { schedule, keep: Number(keep) } }));
  }
  function restore(b: Backup) {
    if (!confirm(`Restore backup #${b.id} (${fmt(b.startedAt)})? Current data is replaced. A safety backup is taken first.`)) return;
    run("restore", () => api(`/databases/${dbId}/restore`, { json: { backupId: b.id } }));
  }

  async function remove() {
    if (!d) return;
    const typed = prompt(`Delete database "${d.name}"? Type its name to confirm.`);
    if (typed !== d.name) return;
    const deleteData = confirm("Also delete its data volume and the backups on this server? Cancel keeps them on disk. Backups uploaded to S3 are always kept.");
    try {
      await api(`/databases/${dbId}${deleteData ? "?deleteData=1" : ""}`, { method: "DELETE" });
      router.go("/");
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  const fmt = (ts?: number) => (ts ? new Date(ts * 1000).toLocaleString() : "");
  const working = $derived(
    d && (d.runtimeStatus === "creating" || (d.runtimeMessage ?? "").startsWith("Restoring") || d.backups.some((b) => b.status === "running")),
  );

  let filled = false;
  onMount(() => {
    api<{ destinations: Destination[] }>("/destinations").then((r) => (dests = r.destinations)).catch(() => {});
    load().then(() => {
      if (d && !filled) {
        schedule = d.backupSchedule;
        keep = String(d.backupKeep);
        publicPort = String(d.publicPort || d.port + 10000);
        allow = d.publicAllow.join(", ");
        destSel = d.backupDestinationId ? String(d.backupDestinationId) : "";
        filled = true;
      }
    });
    const t = setInterval(() => load(), 2000);
    return () => clearInterval(t);
  });

  const tabs = [
    ["connection", "Connection"],
    ["apps", "Linked apps"],
    ["users", "Users"],
    ["backups", "Backups"],
  ] as const;
  const row = "flex items-center justify-between gap-4 px-3 py-2 text-sm";
</script>

{#if !d}
  <p class="text-neutral-500 text-sm">{error || "Loading…"}</p>
{:else}
  <div class="flex items-start justify-between gap-4 mb-2">
    <div>
      <p class="text-xs text-neutral-500">{d.projectName}</p>
      <h1 class="text-xl font-semibold flex items-center gap-3">
        {d.name}
        <StatusBadge status={d.runtimeStatus} />
        {#if d.publicPort}<span class="text-xs rounded-full px-2 py-0.5 bg-amber-950 text-amber-300">public :{d.publicPort}</span>{/if}
        {#if d.runtimeStatus === "running"}<a href="/terminal?kind=database&id={d.id}" use:link class="text-xs font-normal text-neutral-400 hover:text-white border border-neutral-700 rounded px-2 py-0.5">Terminal</a>{/if}
      </h1>
      <p class="text-xs text-neutral-500 mt-1">{d.engineLabel} {d.version}{d.memoryMb ? ` · ${d.memoryMb} MB limit` : ""}</p>
    </div>
    <div class="flex flex-wrap gap-2 text-sm">
      {#if d.runtimeStatus === "missing" || d.runtimeStatus === "failed"}
        <button onclick={() => action("recreate")} disabled={!!busy} class="rounded-md bg-emerald-600 hover:bg-emerald-500 px-3 py-1.5 font-medium disabled:opacity-50">Recreate</button>
      {/if}
      {#if d.desiredStatus === "stopped"}
        <button onclick={() => action("start")} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 disabled:opacity-50">Start</button>
      {:else}
        <button onclick={() => action("restart")} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 disabled:opacity-50">Restart</button>
        <button onclick={() => action("stop")} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 disabled:opacity-50">Stop</button>
      {/if}
      <button onclick={remove} class="rounded-md border border-red-900 text-red-300 hover:bg-red-950 px-3 py-1.5">Delete</button>
    </div>
  </div>

  {#if d.runtimeMessage}<p class="text-sm {d.runtimeStatus === 'failed' || d.runtimeMessage.startsWith('Restore failed') ? 'text-red-300' : 'text-amber-300'} mb-2">{d.runtimeMessage}</p>{/if}
  {#if error}<p class="text-sm text-red-400 mb-2">{error}</p>{/if}
  {#if busy}<p class="text-sm text-neutral-400 mb-2">Working…</p>{/if}

  <nav class="flex gap-1 border-b border-neutral-800 mb-6 mt-4">
    {#each tabs as [id, label]}
      <button
        onclick={() => (tab = id)}
        class="px-3 py-2 text-sm -mb-px border-b-2 {tab === id ? 'border-emerald-500 text-white' : 'border-transparent text-neutral-400 hover:text-white'}"
        >{label}</button
      >
    {/each}
  </nav>

  {#if tab === "connection"}
    <div class="grid gap-8 lg:grid-cols-2 max-w-6xl">
      <section class="space-y-3">
        <h2 class="font-medium">Inside the project</h2>
        <div class="rounded-md border border-neutral-800 divide-y divide-neutral-800">
          <div class={row}><span class="text-neutral-400">Host</span><span class="font-mono">{d.host}</span></div>
          <div class={row}><span class="text-neutral-400">Port</span><span class="font-mono">{d.port}</span></div>
          {#if d.dbName}<div class={row}><span class="text-neutral-400">Database</span><span class="font-mono">{d.dbName}</span></div>{/if}
          {#if d.username}<div class={row}><span class="text-neutral-400">User</span><span class="font-mono">{d.username}</span></div>{/if}
          <div class={row}>
            <span class="text-neutral-400">Password</span>
            {#if creds}<span class="font-mono select-all break-all">{creds.password}</span>
            {:else}<button onclick={async () => (creds = await api(`/databases/${dbId}/credentials`))} class="rounded border border-neutral-700 hover:border-neutral-500 px-2 py-0.5 text-xs">Reveal</button>{/if}
          </div>
          {#if creds?.rootPassword}
            <div class={row}><span class="text-neutral-400">Root password</span><span class="font-mono select-all break-all">{creds.rootPassword}</span></div>
          {/if}
        </div>
        {#if creds}
          <p class="text-xs text-neutral-500">Connection URL</p>
          <div class="rounded-md bg-neutral-900 border border-neutral-800 p-2 font-mono text-xs break-all select-all">{creds.url}</div>
          <button onclick={() => copy(creds!.url, "url")} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1 text-sm">{copied === "url" ? "Copied" : "Copy URL"}</button>
        {/if}
        <p class="text-xs text-neutral-500">Revealing credentials is recorded in the audit log.</p>
      </section>

      <section class="space-y-3">
        <h2 class="font-medium">Public access</h2>
        {#if d.publicPort}
          <p class="text-sm text-amber-300">
            Published on port <strong>{d.publicPort}</strong> of this server{d.publicAllow.length ? `, reachable only from ${d.publicAllow.join(", ")}` : " to anyone"}.
          </p>
          <div class="flex gap-2 items-end">
            <label class="text-sm flex-1 max-w-md">Allowed sources (IPv4 or CIDR; empty = anyone)
              <input bind:value={allow} placeholder="203.0.113.7, 10.0.0.0/8" class="mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm font-mono" /></label>
            <button onclick={saveAllow} disabled={!!busy || allow === d.publicAllow.join(", ")} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Save</button>
          </div>
          <p class="text-xs text-neutral-500">Enforced in the server's iptables (Docker-published ports skip ufw). Containers on this server are not affected.</p>
          <button onclick={() => setPublic(false)} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Make private</button>
        {:else}
          <p class="text-sm text-neutral-400">
            Private: only apps in <strong>{d.projectName}</strong> can connect. Publish a port to reach it from outside, e.g. with a desktop client.
          </p>
          <div class="flex gap-2">
            <input bind:value={publicPort} inputmode="numeric" class="w-28 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm" aria-label="Public port" />
            <button onclick={() => setPublic(true)} disabled={!!busy} class="rounded-md border border-amber-800 text-amber-300 hover:bg-amber-950 px-3 py-1.5 text-sm">Publish port</button>
          </div>
          <p class="text-xs text-neutral-500">The container is recreated on the same data volume (a few seconds of downtime).</p>
        {/if}
      </section>
    </div>
  {:else if tab === "apps"}
    <section class="max-w-3xl space-y-4">
      <p class="text-sm text-neutral-400">Linking writes the connection URL into the app's environment. Redeploy the app to pick it up.</p>
      <div class="rounded-md border border-neutral-800 divide-y divide-neutral-800">
        {#each d.links as l (l.appId)}
          <div class={row}>
            <button onclick={() => router.go(`/apps/${l.appId}`)} class="hover:underline">{l.appName}</button>
            <span class="flex items-center gap-4">
              <span class="font-mono text-neutral-400">{l.envKey}</span>
              <button onclick={() => run("unlink", () => api(`/databases/${dbId}/links/${l.appId}`, { method: "DELETE" }))} class="text-neutral-500 hover:text-red-300 text-xs">Unlink</button>
            </span>
          </div>
        {:else}
          <p class="px-3 py-4 text-sm text-neutral-500">No apps linked.</p>
        {/each}
      </div>
      {#if linkable.length}
        <form onsubmit={addLink} class="flex flex-wrap gap-2 items-end">
          <label class="text-sm">App
            <select bind:value={linkApp} class="block mt-1 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm">
              <option value="" disabled>Choose…</option>
              {#each linkable as a}<option value={String(a.id)}>{a.name}</option>{/each}
            </select></label>
          <label class="text-sm">Variable
            <input bind:value={linkKey} placeholder={d.defaultEnvKey} class="block mt-1 w-44 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm font-mono" /></label>
          <button disabled={!linkApp} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50">Link</button>
        </form>
      {:else if d.apps.length === 0}
        <p class="text-sm text-neutral-500">Create an app in {d.projectName} to link it.</p>
      {/if}
    </section>
  {:else if tab === "users"}
    <section class="space-y-4 max-w-4xl">
      <p class="text-sm text-neutral-400">
        Extra users for apps or people that need less than full access. The owner <span class="font-mono">{d.username || "default"}</span> stays the
        administrator used by Docklet and by "Link to app".
      </p>
      {#if d.runtimeStatus !== "running"}<p class="text-sm text-amber-300">Start the database to change users.</p>{/if}
      <div class="rounded-md border border-neutral-800 divide-y divide-neutral-800">
        {#each d.users as u (u.id)}
          <div class="px-3 py-2 text-sm space-y-2">
            <div class="flex flex-wrap items-center justify-between gap-3">
              <span class="font-mono">{u.username}</span>
              <span class="flex items-center gap-3">
                <select value={u.role} onchange={(e) => setRole(u, (e.target as HTMLSelectElement).value)} disabled={!!busy}
                  class="rounded-md bg-neutral-900 border border-neutral-700 px-2 py-1 text-xs">
                  {#each d.roles as r}<option value={r}>{r}</option>{/each}
                </select>
                <button onclick={() => reveal(u)} class="text-xs text-neutral-300 hover:underline">Credentials</button>
                <button onclick={() => resetPw(u)} class="text-xs text-neutral-300 hover:underline">New password</button>
                <button onclick={() => removeUser(u)} class="text-xs text-neutral-500 hover:text-red-300">Delete</button>
              </span>
            </div>
            <p class="text-xs text-neutral-500">{roleText[u.role]}</p>
            {#if shown[u.id]}
              <div class="rounded bg-neutral-900 border border-neutral-800 p-2 font-mono text-xs break-all select-all">{shown[u.id].url}</div>
              <button onclick={() => copy(shown[u.id].url, "u" + u.id)} class="text-xs text-neutral-300 hover:underline">{copied === "u" + u.id ? "Copied" : "Copy URL"}</button>
            {/if}
          </div>
        {:else}
          <p class="px-3 py-4 text-sm text-neutral-500">No extra users.</p>
        {/each}
      </div>
      <form onsubmit={addUser} class="flex flex-wrap gap-2 items-end">
        <label class="text-sm">User name
          <input bind:value={newUser} placeholder="reporting" class="block mt-1 w-44 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm font-mono" /></label>
        <label class="text-sm">Permissions
          <select bind:value={newRole} class="block mt-1 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm">
            <option value="readonly">Read only</option><option value="readwrite">Read & write</option><option value="admin">Admin</option>
          </select></label>
        <button disabled={!newUser || !!busy || d.runtimeStatus !== "running"} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50">Add user</button>
      </form>
      <p class="text-xs text-neutral-500">{roleText[newRole]}. A strong password is generated; credentials views are audited.</p>
    </section>
  {:else}
    <section class="space-y-5 max-w-4xl">
      <div class="flex flex-wrap items-end gap-4">
        <form onsubmit={savePolicy} class="flex flex-wrap gap-2 items-end">
          <label class="text-sm">Schedule
            <select bind:value={schedule} class="block mt-1 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm">
              <option value="off">Off</option><option value="hourly">Hourly</option><option value="daily">Daily</option><option value="weekly">Weekly</option>
            </select></label>
          <label class="text-sm">Keep
            <input bind:value={keep} inputmode="numeric" class="block mt-1 w-20 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm" /></label>
          <button class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Save</button>
        </form>
        <button
          onclick={() => run("backup", () => api(`/databases/${dbId}/backups`, { method: "POST" }))}
          disabled={!!busy || !!working || d.runtimeStatus !== "running"}
          class="ml-auto rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">Back up now</button
        >
      </div>
      <div class="flex flex-wrap items-end gap-2">
        <label class="text-sm">Also upload to
          <select bind:value={destSel} onchange={setDest} class="block mt-1 rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm">
            <option value="">Nowhere (this server only)</option>
            {#each dests as x}<option value={String(x.id)}>{x.name} ({x.bucket})</option>{/each}
          </select></label>
        {#if dests.length === 0}<a href="/settings" class="text-xs text-emerald-300 hover:underline pb-2">Add an S3 destination in Settings</a>{/if}
      </div>
      <p class="text-xs text-neutral-500">
        Backups are kept on this server{destSel ? " and uploaded to the destination" : ""}; "Keep" applies to both.
        {#if !destSel}Without a destination they are lost with the server.{/if}
      </p>
      <div class="overflow-x-auto rounded-md border border-neutral-800">
        <table class="w-full text-sm">
          <thead class="bg-neutral-900 text-neutral-400 text-left">
            <tr><th class="px-3 py-2 font-medium">#</th><th class="px-3 py-2 font-medium">Started</th><th class="px-3 py-2 font-medium">Type</th><th class="px-3 py-2 font-medium">Status</th><th class="px-3 py-2 font-medium">Stored</th><th class="px-3 py-2 font-medium text-right">Size</th><th></th></tr>
          </thead>
          <tbody>
            {#each d.backups as b (b.id)}
              <tr class="border-t border-neutral-800">
                <td class="px-3 py-2 font-mono text-neutral-500">{b.id}</td>
                <td class="px-3 py-2 whitespace-nowrap">{fmt(b.startedAt)}</td>
                <td class="px-3 py-2 text-neutral-400">{b.trigger}</td>
                <td class="px-3 py-2"><StatusBadge status={b.status === "running" ? "pending" : b.status} />{#if b.error}<div class="text-xs text-red-300 mt-1 max-w-xs truncate" title={b.error}>{b.error}</div>{/if}</td>
                <td class="px-3 py-2 whitespace-nowrap text-xs">
                  {#if b.status === "succeeded"}
                    {#if b.local}<span class="rounded bg-neutral-800 px-1.5 py-0.5 mr-1">server</span>{/if}
                    {#if b.remoteStatus === "uploaded"}<span class="rounded bg-sky-950 text-sky-300 px-1.5 py-0.5">S3</span>
                    {:else if b.remoteStatus === "uploading"}<span class="text-neutral-400">uploading…</span>
                    {:else if b.remoteStatus === "failed"}<span class="text-red-300" title={b.remoteError}>upload failed</span>
                      {#if b.local}<button onclick={() => retryUpload(b)} class="ml-1 underline">retry</button>{/if}{/if}
                  {/if}
                </td>
                <td class="px-3 py-2 text-right tabular-nums">{b.status === "succeeded" ? bytes(b.sizeBytes) : ""}</td>
                <td class="px-3 py-2 text-right whitespace-nowrap">
                  {#if b.status === "succeeded"}
                    <a href="/api/backups/{b.id}/download" class="text-xs text-neutral-300 hover:underline mr-3">Download</a>
                    <button onclick={() => restore(b)} disabled={!!busy || !!working} class="text-xs rounded border border-neutral-700 hover:border-neutral-500 px-2 py-0.5 disabled:opacity-50">Restore</button>
                  {/if}
                </td>
              </tr>
            {:else}
              <tr><td colspan="7" class="px-3 py-6 text-center text-neutral-500">No backups yet.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </section>
  {/if}
{/if}
