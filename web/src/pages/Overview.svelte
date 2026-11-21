<script lang="ts">
  import { onMount } from "svelte";
  import { api, type Project } from "../lib/api";
  import { link, router } from "../lib/router.svelte";
  import StatusBadge from "../lib/StatusBadge.svelte";

  let projects = $state<Project[]>([]);
  let docker = $state(true);
  let loaded = $state(false);
  let newName = $state("");
  let error = $state("");

  async function load() {
    const res = await api<{ projects: Project[]; docker: boolean }>("/projects");
    projects = res.projects;
    docker = res.docker;
    loaded = true;
  }

  async function createProject(e: SubmitEvent) {
    e.preventDefault();
    error = "";
    try {
      await api("/projects", { json: { name: newName.trim() } });
      newName = "";
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function deleteProject(p: Project) {
    if (!confirm(`Delete project "${p.name}"?`)) return;
    try {
      await api(`/projects/${p.id}`, { method: "DELETE" });
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  onMount(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  });
</script>

<div class="flex items-center justify-between mb-6">
  <h1 class="text-xl font-semibold">Projects</h1>
  <form onsubmit={createProject} class="flex gap-2">
    <input
      bind:value={newName}
      placeholder="new-project"
      class="rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm outline-none focus:border-emerald-500"
    />
    <button class="rounded-md bg-emerald-600 hover:bg-emerald-500 px-3 py-1.5 text-sm font-medium">Create project</button>
  </form>
</div>

{#if error}<p class="text-sm text-red-400 mb-4">{error}</p>{/if}
{#if !docker}
  <div class="rounded-md border border-amber-900 bg-amber-950/40 p-3 text-sm text-amber-300 mb-4">
    Docker integration is disabled (<code>-docker-socket off</code>). Apps cannot be deployed.
  </div>
{/if}

{#if loaded && projects.length === 0}
  <div class="rounded-md border border-dashed border-neutral-800 p-10 text-center text-neutral-500">
    No projects yet. Create one to group related apps on a private network.
  </div>
{/if}

<div class="space-y-6">
  {#each projects as p (p.id)}
    <section class="rounded-lg border border-neutral-800">
      <header class="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <h2 class="font-medium">{p.name}</h2>
        <div class="flex gap-2 text-sm">
          <a
            href="/projects/{p.id}/new"
            use:link
            class="rounded-md border border-neutral-700 hover:border-neutral-500 px-2.5 py-1">New app</a
          >
          <a
            href="/projects/{p.id}/new-database"
            use:link
            class="rounded-md border border-neutral-700 hover:border-neutral-500 px-2.5 py-1">New database</a
          >
          {#if p.apps.length === 0 && p.databases.length === 0}
            <button onclick={() => deleteProject(p)} class="text-neutral-500 hover:text-red-300 px-2">Delete</button>
          {/if}
        </div>
      </header>
      {#if p.apps.length === 0 && p.databases.length === 0}
        <p class="px-4 py-4 text-sm text-neutral-500">No apps or databases in this project.</p>
      {:else}
        <ul>
          {#each p.apps as a (a.id)}
            <li class="border-t border-neutral-900 first:border-t-0">
              <button
                onclick={() => router.go(`/apps/${a.id}`)}
                class="w-full text-left px-4 py-3 hover:bg-neutral-900 flex items-center gap-4"
              >
                <span class="font-medium w-40 truncate">{a.name}</span>
                <StatusBadge status={a.runtimeStatus} />
                <span class="text-xs text-neutral-500 font-mono truncate flex-1">{a.image}</span>
                {#if a.kind === "worker"}<span class="text-xs text-neutral-500">worker</span>
                {:else}<span class="text-xs text-neutral-400 truncate">{a.domains.join(", ")}</span>{/if}
              </button>
            </li>
          {/each}
          {#each p.databases as d (d.id)}
            <li class="border-t border-neutral-900 first:border-t-0">
              <button
                onclick={() => router.go(`/databases/${d.id}`)}
                class="w-full text-left px-4 py-3 hover:bg-neutral-900 flex items-center gap-4"
              >
                <span class="font-medium w-40 truncate">{d.name}</span>
                <StatusBadge status={d.runtimeStatus} />
                <span class="text-xs text-neutral-500 truncate flex-1">{d.engineLabel} {d.version}</span>
                {#if d.publicPort}<span class="text-xs text-amber-300">public :{d.publicPort}</span>{/if}
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  {/each}
</div>
