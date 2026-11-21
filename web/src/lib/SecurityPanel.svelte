<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { api } from "./api";

  let { supported }: { supported: boolean } = $props();

  type SSH = { installed: boolean; ports: number[]; permitRootLogin: string; passwordAuthentication: boolean; socketActivated: boolean; managed: boolean; keys: Record<string, number> };
  type Key = { user: string; fingerprint: string; type: string; comment: string };
  type Pending = { kind: string; description: string; deadline: string; secondsLeft: number };
  type Rule = { num: number; to: string; action: string; from: string; v6: boolean; comment?: string; text: string };
  type FW = { installed: boolean; active: boolean; defaultIncoming: string; defaultOutgoing: string; rules: Rule[] };
  type Protected = { port: number; proto: string; why: string };
  type F2B = {
    installed: boolean; running: boolean;
    settings: { enabled: boolean; maxRetry: number; findTimeSec: number; banTimeSec: number; ignoreIps: string[]; configured: boolean };
    jails: { name: string; currentlyFailed: number; totalFailed: number; banned: string[]; totalBanned: number }[];
  };

  let ssh = $state<SSH | null>(null);
  let keys = $state<Key[]>([]);
  let fw = $state<FW | null>(null);
  let prot = $state<Protected[]>([]);
  let f2b = $state<F2B | null>(null);
  let pending = $state<Pending | null>(null);
  let left = $state(0);
  let error = $state("");
  let msg = $state("");
  let busy = $state("");

  async function run(label: string, fn: () => Promise<unknown>, done?: string) {
    error = msg = "";
    busy = label;
    try {
      await fn();
      if (done) msg = done;
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = "";
    }
  }

  // SSH form
  let port = $state("22");
  let rootLogin = $state("prohibit-password");
  let passwordAuth = $state(true);
  let pw = $state("");
  async function loadSSH() {
    const r = await api<{ ssh: SSH; keys: Key[]; pending: Pending | null }>("/server/ssh");
    ssh = r.ssh;
    keys = r.keys;
    setPending(r.pending);
    port = String(ssh.ports.at(-1) ?? 22);
    rootLogin = ssh.permitRootLogin;
    passwordAuth = ssh.passwordAuthentication;
  }
  async function loadFW() {
    const r = await api<{ firewall: FW; protected: Protected[]; pending: Pending | null }>("/server/firewall");
    fw = r.firewall;
    prot = r.protected;
    setPending(r.pending);
  }
  async function loadF2B() {
    f2b = await api<F2B>("/server/fail2ban");
    const s = f2b.settings;
    f2bForm = { enabled: s.configured ? s.enabled : true, maxRetry: String(s.maxRetry), findMin: String(Math.round(s.findTimeSec / 60)), banMin: s.banTimeSec < 0 ? "-1" : String(Math.round(s.banTimeSec / 60)), ignore: s.ignoreIps.join(", ") };
  }
  const loadAll = () => run("load", () => Promise.all([loadSSH(), loadFW(), loadF2B()]));

  // Pending change countdown
  let timer: ReturnType<typeof setInterval> | undefined;
  function setPending(p: Pending | null) {
    pending = p;
    clearInterval(timer);
    if (!p) return;
    const end = Date.now() + p.secondsLeft * 1000;
    left = p.secondsLeft;
    timer = setInterval(() => {
      left = Math.max(0, Math.round((end - Date.now()) / 1000));
      if (left === 0) {
        clearInterval(timer);
        pending = null;
        error = "The change was not confirmed in time, so the server undid it.";
        setTimeout(loadAll, 3000);
      }
    }, 1000);
  }
  onDestroy(() => clearInterval(timer));

  const changed = $derived(
    !!ssh && (Number(port) !== (ssh.ports.length === 1 ? ssh.ports[0] : -1) || rootLogin !== ssh.permitRootLogin || passwordAuth !== ssh.passwordAuthentication),
  );
  const keyCount = $derived(keys.length);
  const saveSSH = () =>
    run("ssh", async () => {
      const r = await api<{ pending: Pending }>("/server/ssh", { method: "PUT", json: { port: Number(port), permitRootLogin: rootLogin, passwordAuthentication: passwordAuth, password: pw } });
      pw = "";
      setPending(r.pending);
      await loadSSH();
    });
  const confirmChange = () => run("confirm", async () => { await api("/server/security/pending/confirm", { method: "POST" }); setPending(null); await Promise.all([loadSSH(), loadFW()]); }, "Change confirmed.");
  const undoChange = () => run("undo", async () => { await api("/server/security/pending/undo", { method: "POST" }); setPending(null); await Promise.all([loadSSH(), loadFW()]); }, "Change undone.");

  // Keys
  let keyUser = $state("root");
  let keyText = $state("");
  const addKey = () => run("key", async () => { keys = (await api<{ keys: Key[] }>("/server/ssh/keys", { json: { user: keyUser, key: keyText } })).keys; keyText = ""; await loadSSH(); }, "Key added.");
  const removeKey = (k: Key) => confirm(`Remove the ${k.type} key ${k.comment || k.fingerprint} of ${k.user}?`) &&
    run("key", async () => { await api("/server/ssh/keys/delete", { json: { user: k.user, fingerprint: k.fingerprint } }); await loadSSH(); }, "Key removed.");
  const keyUsers = $derived(ssh ? Object.keys(ssh.keys) : ["root"]);

  // Firewall
  let fwPw = $state("");
  let rule = $state({ action: "allow", port: "", proto: "tcp", from: "", comment: "" });
  const install = (what: "firewall" | "fail2ban") =>
    run("install-" + what, async () => { await api(`/server/${what}/install`, { method: "POST" }); await (what === "firewall" ? loadFW() : loadF2B()); }, "Installed.");
  const enableFW = () => run("fw", async () => { const r = await api<{ pending: Pending }>("/server/firewall/enable", { json: { password: fwPw } }); fwPw = ""; setPending(r.pending); await loadFW(); });
  const disableFW = () => run("fw", async () => { await api("/server/firewall/disable", { json: { password: fwPw } }); fwPw = ""; await loadFW(); }, "Firewall turned off.");
  const addRule = () => run("rule", async () => { await api("/server/firewall/rules", { json: rule }); rule = { ...rule, port: "", from: "", comment: "" }; await loadFW(); }, "Rule added.");
  const delRule = (r: Rule) => confirm(`Delete rule ${r.num}: ${r.text}?`) && run("rule", async () => { await api("/server/firewall/rules/delete", { json: { num: r.num, text: r.text } }); await loadFW(); }, "Rule deleted.");
  const setDefault = (incoming: string, outgoing: string) => run("rule", async () => { await api("/server/firewall/defaults", { method: "PUT", json: { incoming, outgoing } }); await loadFW(); }, "Default policy changed.");

  // Fail2Ban
  let f2bForm = $state({ enabled: true, maxRetry: "5", findMin: "10", banMin: "60", ignore: "" });
  const saveF2B = () =>
    run("f2b", async () => {
      f2b = await api<F2B>("/server/fail2ban", { method: "PUT", json: {
        enabled: f2bForm.enabled, maxRetry: Number(f2bForm.maxRetry), findTimeSec: Number(f2bForm.findMin) * 60,
        banTimeSec: Number(f2bForm.banMin) < 0 ? -1 : Number(f2bForm.banMin) * 60,
        ignoreIps: f2bForm.ignore.split(/[\s,]+/).filter(Boolean),
      } });
    }, "Fail2Ban settings saved.");
  const unban = (jail: string, ip: string) => run("unban", async () => { await api("/server/fail2ban/unban", { json: { jail, ip } }); await loadF2B(); }, `${ip} unbanned.`);

  onMount(loadAll);

  const mmss = (s: number) => `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  const input = "rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm outline-none focus:border-emerald-500";
  const btn = "rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50";
  const card = "rounded-lg border border-neutral-800 p-4 space-y-3";
</script>

{#if pending}
  <div class="rounded-lg border border-amber-700 bg-amber-950/40 p-4 mb-4 max-w-6xl space-y-2">
    <p class="text-sm"><strong>Confirm this change:</strong> {pending.description}.</p>
    <p class="text-sm text-neutral-300">
      {#if pending.kind === "ssh"}Open a <em>new</em> SSH session with the new settings (keep your current one open). If it works, confirm.{:else}If you can still use this page and SSH, confirm.{/if}
      Otherwise the server undoes it by itself in <strong class="font-mono">{mmss(left)}</strong>.
    </p>
    <div class="flex gap-2">
      <button onclick={confirmChange} disabled={!!busy} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">It works, keep it</button>
      <button onclick={undoChange} disabled={!!busy} class={btn}>Undo now</button>
    </div>
  </div>
{/if}
{#if error}<p class="text-sm text-red-400 mb-3">{error}</p>{/if}
{#if msg}<p class="text-sm text-emerald-300 mb-3">{msg}</p>{/if}

<div class="grid gap-4 lg:grid-cols-2 max-w-6xl">
  <section class={card}>
    <h2 class="font-medium">SSH</h2>
    {#if !ssh}
      <p class="text-sm text-neutral-500">{busy ? "Loading…" : ""}</p>
    {:else if !ssh.installed}
      <p class="text-sm text-neutral-400">OpenSSH server is not installed.</p>
    {:else}
      <p class="text-sm text-neutral-400">Listening on port {ssh.ports.join(", ")}{ssh.socketActivated ? " (socket activated)" : ""}.</p>
      <div class="grid grid-cols-[10rem_1fr] gap-2 items-center text-sm">
        <span>Port</span><input bind:value={port} inputmode="numeric" class="{input} w-28" />
        <span>Root login</span>
        <select bind:value={rootLogin} class="{input} w-fit">
          <option value="prohibit-password">SSH key only</option>
          <option value="yes">Key or password</option>
          <option value="no">Not allowed</option>
        </select>
        <span>Password login</span>
        <label class="flex items-center gap-2"><input type="checkbox" bind:checked={passwordAuth} /> {passwordAuth ? "Allowed" : "Off — SSH keys only"}</label>
      </div>
      {#if !passwordAuth && keyCount === 0}<p class="text-xs text-amber-300">Add an SSH key below first: without one nobody could log in.</p>{/if}
      {#if changed}
        <div class="flex gap-2 items-end">
          <label class="text-sm">Your password<input type="password" bind:value={pw} autocomplete="current-password" class="mt-1 {input}" /></label>
          <button onclick={saveSSH} disabled={!supported || !pw || !!busy || !!pending} class={btn}>{busy === "ssh" ? "Applying…" : "Apply"}</button>
        </div>
        <p class="text-xs text-neutral-500">A new port is opened next to the old one (and in the firewall). You then have 5 minutes to check a new SSH login and confirm; if you don't, the server goes back to the old settings by itself.</p>
      {/if}
    {/if}
  </section>

  <section class={card}>
    <h2 class="font-medium">SSH keys</h2>
    {#if keys.length}
      <ul class="text-sm divide-y divide-neutral-800">
        {#each keys as k (k.user + k.fingerprint)}
          <li class="flex items-center justify-between gap-2 py-1.5">
            <span class="min-w-0"><span class="text-neutral-400">{k.user}</span> · {k.type} · {k.comment || "no comment"}<br /><span class="font-mono text-xs text-neutral-500 break-all">{k.fingerprint}</span></span>
            <button onclick={() => removeKey(k)} disabled={!supported || !!busy} class="text-xs text-red-300 hover:underline">Remove</button>
          </li>
        {/each}
      </ul>
    {:else}
      <p class="text-sm text-neutral-400">No keys for root or sudo users.</p>
    {/if}
    <div class="space-y-2">
      <textarea bind:value={keyText} rows="2" placeholder="ssh-ed25519 AAAA… you@laptop" class="w-full {input} font-mono text-xs"></textarea>
      <div class="flex gap-2 items-center">
        <select bind:value={keyUser} class={input}>{#each keyUsers as u}<option>{u}</option>{/each}</select>
        <button onclick={addKey} disabled={!supported || !keyText.trim() || !!busy} class={btn}>Add key</button>
      </div>
    </div>
  </section>

  <section class="{card} lg:col-span-2">
    <div class="flex items-center justify-between">
      <h2 class="font-medium">Firewall (ufw)</h2>
      {#if fw?.installed}<span class="text-xs rounded px-2 py-0.5 {fw.active ? 'bg-emerald-900 text-emerald-200' : 'bg-neutral-800 text-neutral-300'}">{fw.active ? "On" : "Off"}</span>{/if}
    </div>
    {#if !fw}
      <p class="text-sm text-neutral-500">{busy ? "Loading…" : ""}</p>
    {:else if !fw.installed}
      <p class="text-sm text-neutral-400">ufw is not installed.</p>
      <button onclick={() => install("firewall")} disabled={!supported || !!busy} class={btn}>{busy === "install-firewall" ? "Installing…" : "Install ufw"}</button>
    {:else}
      <p class="text-xs text-neutral-400">Always kept open: {prot.map((p) => `${p.port}/${p.proto} (${p.why})`).join(", ")}.
        Ports published by Docker (Caddy, the panel, public databases) skip ufw; restrict a database's public port on its page instead.</p>
      <div class="flex flex-wrap gap-2 items-end">
        <label class="text-sm">Your password<input type="password" bind:value={fwPw} autocomplete="current-password" class="mt-1 {input}" /></label>
        {#if fw.active}
          <button onclick={disableFW} disabled={!supported || !fwPw || !!busy} class="{btn} text-red-300">Turn off</button>
        {:else}
          <button onclick={enableFW} disabled={!supported || !fwPw || !!busy || !!pending} class={btn}>Turn on</button>
        {/if}
        {#if fw.active}
          <label class="text-sm ml-4">Incoming by default
            <select value={fw.defaultIncoming} onchange={(e) => setDefault(e.currentTarget.value, fw!.defaultOutgoing)} class="mt-1 block {input}">
              <option value="deny">deny</option><option value="reject">reject</option><option value="allow">allow</option>
            </select></label>
          <label class="text-sm">Outgoing by default
            <select value={fw.defaultOutgoing} onchange={(e) => setDefault(fw!.defaultIncoming, e.currentTarget.value)} class="mt-1 block {input}">
              <option value="allow">allow</option><option value="deny">deny</option><option value="reject">reject</option>
            </select></label>
        {/if}
      </div>

      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead class="text-left text-neutral-400 text-xs"><tr><th class="py-1 pr-3">#</th><th class="pr-3">To</th><th class="pr-3">Action</th><th class="pr-3">From</th><th class="pr-3">Comment</th><th></th></tr></thead>
          <tbody class="divide-y divide-neutral-800">
            {#each fw.rules as r (r.num)}
              <tr>
                <td class="py-1 pr-3 text-neutral-500">{r.num}</td>
                <td class="pr-3 font-mono text-xs">{r.to}{r.v6 ? " (v6)" : ""}</td>
                <td class="pr-3 {r.action.startsWith('ALLOW') ? 'text-emerald-300' : r.action.startsWith('LIMIT') ? 'text-amber-300' : 'text-red-300'}">{r.action}</td>
                <td class="pr-3 font-mono text-xs">{r.from}</td>
                <td class="pr-3 text-neutral-400">{r.comment ?? ""}</td>
                <td class="text-right"><button onclick={() => delRule(r)} disabled={!supported || !!busy} class="text-xs text-red-300 hover:underline">Delete</button></td>
              </tr>
            {:else}
              <tr><td colspan="6" class="py-2 text-neutral-500">No rules.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
      <form onsubmit={(e) => { e.preventDefault(); addRule(); }} class="flex flex-wrap gap-2 items-end">
        <label class="text-sm">Action<select bind:value={rule.action} class="mt-1 block {input}"><option>allow</option><option>deny</option><option>reject</option><option>limit</option></select></label>
        <label class="text-sm">Port or range<input bind:value={rule.port} placeholder="8080 or 6000:6100" class="mt-1 block w-40 {input}" /></label>
        <label class="text-sm">Protocol<select bind:value={rule.proto} class="mt-1 block {input}"><option>tcp</option><option>udp</option><option>any</option></select></label>
        <label class="text-sm">From<input bind:value={rule.from} placeholder="anywhere, or IP / CIDR" class="mt-1 block w-48 {input}" /></label>
        <label class="text-sm">Comment<input bind:value={rule.comment} class="mt-1 block w-40 {input}" /></label>
        <button disabled={!supported || !rule.port || !!busy} class={btn}>Add rule</button>
      </form>
      <p class="text-xs text-neutral-500">"limit" allows a port but blocks an address after 6 connections in 30 seconds. Deny rules are added at the top so they win over allows.</p>
    {/if}
  </section>

  <section class="{card} lg:col-span-2">
    <div class="flex items-center justify-between">
      <h2 class="font-medium">Fail2Ban</h2>
      {#if f2b?.installed}<span class="text-xs rounded px-2 py-0.5 {f2b.running ? 'bg-emerald-900 text-emerald-200' : 'bg-neutral-800 text-neutral-300'}">{f2b.running ? "Running" : "Stopped"}</span>{/if}
    </div>
    {#if !f2b}
      <p class="text-sm text-neutral-500">{busy ? "Loading…" : ""}</p>
    {:else if !f2b.installed}
      <p class="text-sm text-neutral-400">Fail2Ban bans addresses that keep failing to log in over SSH.</p>
      <button onclick={() => install("fail2ban")} disabled={!supported || !!busy} class={btn}>{busy === "install-fail2ban" ? "Installing… (up to a minute)" : "Install Fail2Ban"}</button>
    {:else}
      <form onsubmit={(e) => { e.preventDefault(); saveF2B(); }} class="flex flex-wrap gap-3 items-end">
        <label class="text-sm flex items-center gap-2 pb-1.5"><input type="checkbox" bind:checked={f2bForm.enabled} /> Protect SSH</label>
        <label class="text-sm">Failures before ban<input bind:value={f2bForm.maxRetry} inputmode="numeric" class="mt-1 block w-24 {input}" /></label>
        <label class="text-sm">Within (minutes)<input bind:value={f2bForm.findMin} inputmode="numeric" class="mt-1 block w-24 {input}" /></label>
        <label class="text-sm">Ban for (minutes, -1 = forever)<input bind:value={f2bForm.banMin} class="mt-1 block w-24 {input}" /></label>
        <label class="text-sm flex-1 min-w-48">Never ban<input bind:value={f2bForm.ignore} placeholder="your office IP, 10.0.0.0/8" class="mt-1 block w-full {input}" /></label>
        <button disabled={!supported || !!busy} class={btn}>Save</button>
      </form>
      {#each f2b.jails as j (j.name)}
        <div class="text-sm">
          <p><span class="font-mono">{j.name}</span>: {j.currentlyFailed} failing now, {j.totalBanned} banned in total.</p>
          {#if j.banned.length}
            <ul class="mt-1 flex flex-wrap gap-2">
              {#each j.banned as ip}
                <li class="rounded border border-neutral-700 px-2 py-0.5 font-mono text-xs flex items-center gap-2">{ip}<button onclick={() => unban(j.name, ip)} disabled={!!busy} class="text-emerald-300 hover:underline">unban</button></li>
              {/each}
            </ul>
          {:else}<p class="text-neutral-500 text-xs">No addresses banned right now.</p>{/if}
        </div>
      {/each}
      <button onclick={() => run("load", loadF2B)} disabled={!!busy} class="{btn} text-xs">Refresh</button>
    {/if}
  </section>
</div>
