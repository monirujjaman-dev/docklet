<script lang="ts">
  import { api } from "../lib/api";
  import { router } from "../lib/router.svelte";
  import { parseEnv } from "../lib/env";
  import Field from "../lib/Field.svelte";

  let { projectId }: { projectId: string } = $props();

  let name = $state("");
  let kind = $state<"web" | "worker">("web");
  let command = $state("");
  let source = $state<"image" | "git">("image");
  let image = $state("");
  let gitUrl = $state("");
  let gitBranch = $state("main");
  let gitContext = $state(".");
  let gitDockerfile = $state("Dockerfile");
  let autoDeploy = $state(true);
  const isSSH = $derived(/^(ssh:\/\/|[\w.-]+@[\w.-]+:)/.test(gitUrl.trim()));
  let port = $state("");
  let domains = $state("");
  let envText = $state("");
  let memoryMb = $state("");
  let cpus = $state("");
  let restartPolicy = $state("unless-stopped");
  let error = $state("");
  let busy = $state(false);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = "";
    const { env, errors } = parseEnv(envText);
    if (errors.length) {
      error = errors.join("; ");
      return;
    }
    busy = true;
    try {
      const res = await api<{ id: number }>(`/projects/${projectId}/apps`, {
        json: {
          name: name.trim(),
          kind,
          command: command.trim(),
          ...(source === "image"
            ? { image: image.trim() }
            : {
                git: {
                  url: gitUrl.trim(),
                  branch: gitBranch.trim(),
                  context: gitContext.trim(),
                  dockerfile: gitDockerfile.trim(),
                  autoDeploy,
                },
              }),
          port: port ? Number(port) : 0,
          domains: kind === "web" ? domains.split(/[\s,]+/).filter(Boolean) : [],
          env,
          memoryMb: memoryMb ? Number(memoryMb) : 0,
          cpuMillis: cpus ? Math.round(Number(cpus) * 1000) : 0,
          restartPolicy,
        },
      });
      router.go(`/apps/${res.id}`);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<h1 class="text-xl font-semibold mb-6">New app</h1>

<form onsubmit={submit} class="max-w-2xl space-y-4">
  <div class="grid grid-cols-2 gap-3 max-w-md" role="radiogroup" aria-label="Kind">
    {#each [["web", "Web service", "Gets domains and HTTPS"], ["worker", "Background worker", "No public route; queues, bots, consumers"]] as [id, label, sub]}
      <button
        type="button"
        role="radio"
        aria-checked={kind === id}
        onclick={() => (kind = id as "web" | "worker")}
        class="rounded-lg border p-3 text-left {kind === id ? 'border-emerald-500 bg-emerald-950/30' : 'border-neutral-800 hover:border-neutral-600'}"
      >
        <span class="block font-medium text-sm">{label}</span>
        <span class="block text-xs text-neutral-500 mt-0.5">{sub}</span>
      </button>
    {/each}
  </div>
  <div class="grid gap-4 md:grid-cols-2">
    <Field label="Name" bind:value={name} hint="Lowercase letters, digits and '-'; also the hostname on the project network" />
    <div class="md:col-span-2">
      <span class="block text-sm text-neutral-300 mb-1">Source</span>
      <div class="inline-flex rounded-md border border-neutral-700 p-0.5" role="radiogroup" aria-label="Source">
        {#each [["image", "Registry image"], ["git", "Git repository"]] as [id, label]}
          <button
            type="button"
            role="radio"
            aria-checked={source === id}
            onclick={() => (source = id as "image" | "git")}
            class="px-3 py-1.5 text-sm rounded {source === id ? 'bg-neutral-800 text-white' : 'text-neutral-400 hover:text-white'}"
            >{label}</button
          >
        {/each}
      </div>
    </div>
    {#if source === "image"}
      <Field label="Image" bind:value={image} hint="e.g. nginx:alpine or ghcr.io/org/app:1.2" />
    {:else}
      <Field label="Repository URL" bind:value={gitUrl} hint="https://github.com/org/repo.git, or git@github.com:org/repo.git for private repos" />
      <Field label="Branch" bind:value={gitBranch} />
      <Field label="Build context" bind:value={gitContext} hint="Directory inside the repo, usually ." />
      <Field label="Dockerfile" bind:value={gitDockerfile} hint="Path inside the repo" />
      <label class="flex items-center gap-2 text-sm text-neutral-300 md:col-span-2">
        <input type="checkbox" bind:checked={autoDeploy} /> Deploy automatically on every push (webhook)
      </label>
      {#if isSSH}
        <p class="md:col-span-2 text-sm text-amber-300">
          Private repository: after creating the app, add its deploy key to the repository, then press Deploy.
        </p>
      {/if}
    {/if}
    {#if kind === "web"}
      <Field label="Container port" bind:value={port} inputmode="numeric" hint="Leave empty to use the port the image exposes" />
      <Field label="Domains" bind:value={domains} hint="Comma or space separated; DNS must point here" />
    {:else}
      <Field label="Internal port (optional)" bind:value={port} inputmode="numeric" hint="Only if other apps call it; then it must accept TCP connections" />
    {/if}
    <div class="md:col-span-2">
      <Field label="Command (optional)" bind:value={command} hint={"Replaces the image's CMD, e.g. php artisan queue:work. No shell: use sh -c '…' for pipes or $VARS"} />
    </div>
    <Field label="Memory limit (MB)" bind:value={memoryMb} inputmode="numeric" hint="Empty = no limit" />
    <Field label="CPU limit (cores)" bind:value={cpus} hint="e.g. 0.5; empty = no limit" />
  </div>
  <label class="block">
    <span class="block text-sm text-neutral-300 mb-1">Restart policy</span>
    <select bind:value={restartPolicy} class="rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm">
      <option value="unless-stopped">unless-stopped (recommended)</option>
      <option value="always">always</option>
      <option value="on-failure">on-failure</option>
      <option value="no">no</option>
    </select>
  </label>
  <label class="block">
    <span class="block text-sm text-neutral-300 mb-1">Environment variables</span>
    <textarea
      bind:value={envText}
      rows="6"
      placeholder={"DATABASE_URL=postgres://…\nLOG_LEVEL=info"}
      class="w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm font-mono outline-none focus:border-emerald-500"
    ></textarea>
    <span class="block text-xs text-neutral-500 mt-1">One KEY=VALUE per line. Values are encrypted and can't be read back.</span>
  </label>
  {#if error}<p class="text-sm text-red-400">{error}</p>{/if}
  <button disabled={busy} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-4 py-2 text-sm font-medium">
    {busy ? "Creating…" : source === "git" && isSSH ? "Create app" : "Create and deploy"}
  </button>
</form>
