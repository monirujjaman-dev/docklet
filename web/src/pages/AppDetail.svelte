<script lang="ts">
  import { onMount, tick } from "svelte";
  import { api, type AppDetail, type Deployment } from "../lib/api";
  import { router } from "../lib/router.svelte";
  import { parseEnv } from "../lib/env";
  import StatusBadge from "../lib/StatusBadge.svelte";
  import MetricsPanel from "../lib/MetricsPanel.svelte";
  import GitSource from "../lib/GitSource.svelte";

  let { appId }: { appId: string } = $props();

  let app = $state<AppDetail | null>(null);
  let tab = $state<"overview" | "metrics" | "env" | "deployments" | "logs">("overview");
  let error = $state("");
  let busy = $state("");

  async function load() {
    try {
      app = await api<AppDetail>(`/apps/${appId}`);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  const inProgress = $derived(app?.deployments.some((d) => d.status === "queued" || d.status === "running") ?? false);

  async function action(name: "deploy" | "start" | "stop" | "restart") {
    error = "";
    busy = name;
    try {
      if (name === "deploy") {
        await api(`/apps/${appId}/deploy`, { method: "POST" });
        tab = "deployments";
      } else {
        app = await api<AppDetail>(`/apps/${appId}/${name}`, { method: "POST" });
      }
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = "";
    }
  }

  async function remove() {
    if (!app || !confirm(`Delete app "${app.name}" and its containers? This cannot be undone.`)) return;
    try {
      await api(`/apps/${appId}`, { method: "DELETE" });
      router.go("/");
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  // --- Overview: config and domains ---
  let cfg = $state({ image: "", port: "", memoryMb: "", cpus: "", restartPolicy: "unless-stopped" });
  let domainsText = $state("");
  let saveMsg = $state("");

  function fillForms(a: AppDetail) {
    cfg = {
      image: a.image,
      port: a.port ? String(a.port) : "",
      memoryMb: a.memoryMb ? String(a.memoryMb) : "",
      cpus: a.cpuMillis ? String(a.cpuMillis / 1000) : "",
      restartPolicy: a.restartPolicy,
    };
    domainsText = a.domains.join(", ");
  }

  async function saveConfig(deploy: boolean) {
    error = saveMsg = "";
    try {
      await api(`/apps/${appId}`, {
        method: "PATCH",
        json: {
          ...(app?.sourceType === "git" ? {} : { image: cfg.image.trim() }),
          port: cfg.port ? Number(cfg.port) : 0,
          memoryMb: cfg.memoryMb ? Number(cfg.memoryMb) : 0,
          cpuMillis: cfg.cpus ? Math.round(Number(cfg.cpus) * 1000) : 0,
          restartPolicy: cfg.restartPolicy,
        },
      });
      if (deploy) await action("deploy");
      else saveMsg = "Saved. Deploy to apply.";
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function saveDomains() {
    error = saveMsg = "";
    try {
      app = await api<AppDetail>(`/apps/${appId}/domains`, {
        method: "PUT",
        json: { domains: domainsText.split(/[\s,]+/).filter(Boolean) },
      });
      saveMsg = "Domains updated. Caddy applies the change within seconds.";
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  // --- Environment ---
  let envText = $state("");
  let envMsg = $state("");

  async function addEnv(e: SubmitEvent) {
    e.preventDefault();
    envMsg = "";
    const { env, errors } = parseEnv(envText);
    if (errors.length) {
      envMsg = errors.join("; ");
      return;
    }
    try {
      app = await api<AppDetail>(`/apps/${appId}/env`, { method: "PUT", json: { env } });
      envText = "";
      envMsg = "Saved. Redeploy for the app to see the change.";
    } catch (err) {
      envMsg = err instanceof Error ? err.message : String(err);
    }
  }

  async function unsetEnv(key: string) {
    if (!confirm(`Remove ${key}?`)) return;
    app = await api<AppDetail>(`/apps/${appId}/env`, { method: "PUT", json: { env: { [key]: null } } });
    envMsg = "Removed. Redeploy for the app to see the change.";
  }

  // --- Deployments ---
  async function rollback(d: Deployment) {
    if (!confirm(`Roll back to deployment #${d.id}${d.commitSha ? ` (${d.commitSha.slice(0, 7)})` : ""}? Its image is redeployed as-is, without a build.`)) return;
    error = "";
    try {
      const res = await api<{ deploymentId: number }>(`/apps/${appId}/rollback`, { json: { deploymentId: d.id } });
      await load();
      await showDeployment(res.deploymentId);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  let openDep = $state<Deployment | null>(null);
  async function showDeployment(id: number) {
    openDep = await api<Deployment>(`/deployments/${id}`);
  }

  // --- Logs (SSE) ---
  let logLines = $state<{ stream: string; text: string }[]>([]);
  let logEnd = $state("");
  let logBox: HTMLDivElement | undefined = $state();
  let es: EventSource | null = null;
  let follow = $state(true);

  function openLogs() {
    closeLogs();
    logLines = [];
    logEnd = "";
    es = new EventSource(`/api/apps/${appId}/logs?tail=300&follow=1`);
    es.onmessage = async (ev) => {
      const line = JSON.parse(ev.data);
      logLines.push(line);
      if (logLines.length > 2000) logLines.splice(0, logLines.length - 2000);
      if (follow) {
        await tick();
        logBox?.scrollTo({ top: logBox.scrollHeight });
      }
    };
    es.addEventListener("end", (ev) => {
      logEnd = JSON.parse((ev as MessageEvent).data).message;
      closeLogs();
    });
    es.onerror = () => {
      if (es?.readyState === EventSource.CLOSED) logEnd = "Log stream closed";
    };
  }
  function closeLogs() {
    es?.close();
    es = null;
  }

  $effect(() => {
    if (tab === "logs") openLogs();
    else closeLogs();
  });

  let filled = false;
  onMount(() => {
    load().then(() => {
      if (app && !filled) {
        fillForms(app);
        filled = true;
      }
    });
    const t = setInterval(async () => {
      await load();
      if (openDep && (openDep.status === "queued" || openDep.status === "running")) await showDeployment(openDep.id);
    }, 2500);
    return () => {
      clearInterval(t);
      closeLogs();
    };
  });

  const fmt = (ts?: number) => (ts ? new Date(ts * 1000).toLocaleString() : "");
  const tabs = [
    ["overview", "Overview"],
    ["metrics", "Metrics"],
    ["env", "Environment"],
    ["deployments", "Deployments"],
    ["logs", "Logs"],
  ] as const;
</script>

{#if !app}
  <p class="text-neutral-500 text-sm">{error || "Loading…"}</p>
{:else}
  <div class="flex items-start justify-between gap-4 mb-2">
    <div>
      <p class="text-xs text-neutral-500">{app.projectName}</p>
      <h1 class="text-xl font-semibold flex items-center gap-3">
        {app.name}
        <StatusBadge status={app.runtimeStatus} />
        {#if inProgress}<StatusBadge status="pending" />{/if}
      </h1>
      {#if app.sourceType === "git" && app.git}
        <p class="text-xs text-neutral-500 font-mono mt-1">{app.git.url} · {app.git.branch}</p>
      {:else}
        <p class="text-xs text-neutral-500 font-mono mt-1">{app.image}</p>
      {/if}
    </div>
    <div class="flex flex-wrap gap-2 text-sm">
      <button
        onclick={() => action("deploy")}
        disabled={!!busy}
        class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 font-medium"
        >{busy === "deploy" ? "Queuing…" : "Deploy"}</button
      >
      {#if app.desiredStatus === "running"}
        <button onclick={() => action("restart")} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 disabled:opacity-50">Restart</button>
        <button onclick={() => action("stop")} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 disabled:opacity-50">Stop</button>
      {:else}
        <button onclick={() => action("start")} disabled={!!busy} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 disabled:opacity-50">Start</button>
      {/if}
      <button onclick={remove} class="rounded-md border border-red-900 text-red-300 hover:bg-red-950 px-3 py-1.5">Delete</button>
    </div>
  </div>

  {#if app.runtimeMessage}<p class="text-sm text-amber-300 mb-2">{app.runtimeMessage}</p>{/if}
  {#if app.sourceType === "git" && !app.buildsAvailable}
    <p class="text-sm text-amber-300 mb-2">Builds are unavailable on this server: install git, openssh-client and the docker CLI with buildx.</p>
  {/if}
  {#if error}<p class="text-sm text-red-400 mb-2">{error}</p>{/if}

  <nav class="flex gap-1 border-b border-neutral-800 mb-6 mt-4">
    {#each tabs as [id, label]}
      <button
        onclick={() => (tab = id)}
        class="px-3 py-2 text-sm -mb-px border-b-2 {tab === id ? 'border-emerald-500 text-white' : 'border-transparent text-neutral-400 hover:text-white'}"
        >{label}</button
      >
    {/each}
  </nav>

  {#if tab === "overview"}
    <div class="grid gap-8 md:grid-cols-2 max-w-5xl">
      <section class="space-y-3">
        <h2 class="font-medium">Domains</h2>
        <input
          bind:value={domainsText}
          placeholder="app.example.com, www.example.com"
          class="w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm outline-none focus:border-emerald-500"
        />
        <button onclick={saveDomains} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Save domains</button>
        {#each app.domains as d}
          <p class="text-sm"><a class="text-emerald-300 hover:underline" href="https://{d}" target="_blank" rel="noreferrer">https://{d}</a></p>
        {/each}
      </section>

      <section class="space-y-3">
        <h2 class="font-medium">Configuration</h2>
        <div class="grid grid-cols-2 gap-3 text-sm">
          {#if app.sourceType !== "git"}
            <label class="col-span-2">Image
              <input bind:value={cfg.image} class="mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 font-mono" /></label>
          {/if}
          <label>Port
            <input bind:value={cfg.port} placeholder="auto" class="mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2" /></label>
          <label>Restart
            <select bind:value={cfg.restartPolicy} class="mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2">
              <option>unless-stopped</option><option>always</option><option>on-failure</option><option>no</option>
            </select></label>
          <label>Memory (MB)
            <input bind:value={cfg.memoryMb} placeholder="no limit" class="mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2" /></label>
          <label>CPU (cores)
            <input bind:value={cfg.cpus} placeholder="no limit" class="mt-1 w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2" /></label>
        </div>
        <div class="flex gap-2">
          <button onclick={() => saveConfig(true)} class="rounded-md bg-emerald-600 hover:bg-emerald-500 px-3 py-1.5 text-sm font-medium">Save and deploy</button>
          <button onclick={() => saveConfig(false)} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Save only</button>
        </div>
      </section>
    </div>
    {#if saveMsg}<p class="text-sm text-emerald-300 mt-4">{saveMsg}</p>{/if}
    {#if app.sourceType === "git" && app.git}
      <div class="mt-10 pt-6 border-t border-neutral-800">
        <GitSource {app} onchange={(a) => (app = a)} />
      </div>
    {/if}
  {:else if tab === "metrics"}
    {#if app.runtimeStatus !== "running"}
      <p class="text-sm text-neutral-500 mb-4">The app is not running; live data is paused and stored history is shown.</p>
    {/if}
    <MetricsPanel scope="app:{app.id}" />
  {:else if tab === "env"}
    <section class="max-w-3xl space-y-4">
      <p class="text-sm text-neutral-400">Values are encrypted at rest and cannot be displayed. Setting an existing key replaces its value.</p>
      <div class="rounded-md border border-neutral-800 divide-y divide-neutral-800">
        {#each app.env as e (e.key)}
          <div class="flex items-center justify-between px-3 py-2 text-sm">
            <span class="font-mono">{e.key}</span>
            <span class="flex items-center gap-4 text-neutral-500">
              <span class="font-mono">••••••••</span>
              <span class="text-xs">{fmt(e.updatedAt)}</span>
              <button onclick={() => unsetEnv(e.key)} class="text-neutral-500 hover:text-red-300">Remove</button>
            </span>
          </div>
        {:else}
          <p class="px-3 py-4 text-sm text-neutral-500">No environment variables.</p>
        {/each}
      </div>
      <form onsubmit={addEnv} class="space-y-2">
        <textarea bind:value={envText} rows="4" placeholder="KEY=value" class="w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm font-mono outline-none focus:border-emerald-500"></textarea>
        <button class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Set variables</button>
      </form>
      {#if envMsg}<p class="text-sm text-neutral-300">{envMsg}</p>{/if}
    </section>
  {:else if tab === "deployments"}
    <div class="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.3fr)]">
      <div class="rounded-md border border-neutral-800 divide-y divide-neutral-800">
        {#each app.deployments as d (d.id)}
          <div class="flex items-stretch hover:bg-neutral-900 {openDep?.id === d.id ? 'bg-neutral-900' : ''}">
            <button onclick={() => showDeployment(d.id)} class="flex-1 min-w-0 text-left px-3 py-2.5">
              <div class="flex items-center gap-3 text-sm">
                <span class="font-mono text-neutral-500">#{d.id}</span>
                <StatusBadge status={d.status} />
                {#if d.current}<span class="text-xs text-emerald-400">current</span>{/if}
                {#if d.source === "rollback"}<span class="text-xs text-neutral-400">rollback of #{d.rollbackOf}</span>{/if}
                <span class="ml-auto text-xs text-neutral-500">{fmt(d.createdAt)}</span>
              </div>
              {#if d.commitSha}
                <div class="text-xs text-neutral-300 mt-1 truncate">
                  <span class="font-mono text-neutral-500">{d.commitSha.slice(0, 7)}</span> {d.commitMessage}
                </div>
              {/if}
              <div class="text-xs text-neutral-500 font-mono mt-1 truncate">{d.image || (d.status === "queued" || d.status === "running" ? "building…" : "no image")} · by {d.createdBy}</div>
              {#if d.error}<div class="text-xs text-red-300 mt-1 truncate">{d.error}</div>{/if}
            </button>
            {#if d.rollbackable}
              <button onclick={() => rollback(d)} class="self-center mr-3 rounded-md border border-neutral-700 hover:border-neutral-500 px-2 py-1 text-xs whitespace-nowrap">Roll back</button>
            {/if}
          </div>
        {:else}
          <p class="px-3 py-4 text-sm text-neutral-500">No deployments yet.</p>
        {/each}
      </div>
      <div>
        {#if openDep}
          <pre class="rounded-md bg-black border border-neutral-800 p-3 text-xs leading-relaxed overflow-auto max-h-[70vh] whitespace-pre-wrap">{openDep.log || (openDep.status === "queued" ? "Waiting in queue…" : "Starting…")}</pre>
        {:else}
          <p class="text-sm text-neutral-500">Select a deployment to see its log.</p>
        {/if}
      </div>
    </div>
  {:else if tab === "logs"}
    <div class="flex items-center justify-between mb-2 text-sm">
      <label class="flex items-center gap-2 text-neutral-400"><input type="checkbox" bind:checked={follow} /> Follow</label>
      <button onclick={openLogs} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1">Reconnect</button>
    </div>
    <div bind:this={logBox} class="rounded-md bg-black border border-neutral-800 p-3 text-xs font-mono leading-relaxed overflow-auto h-[65vh]">
      {#each logLines as l, i (i)}
        <div class="whitespace-pre-wrap {l.stream === 'stderr' ? 'text-amber-200' : 'text-neutral-200'}">{l.text}</div>
      {/each}
      {#if logEnd}<div class="text-neutral-500 mt-2">— {logEnd} —</div>{/if}
    </div>
  {/if}
{/if}
