<script lang="ts">
  import { onMount } from "svelte";
  import { api, type VersionInfo } from "./lib/api";

  let healthy = $state<boolean | null>(null);
  let info = $state<VersionInfo | null>(null);

  onMount(async () => {
    try {
      const [h, v] = await Promise.all([
        api<{ status: string }>("/health"),
        api<VersionInfo>("/version"),
      ]);
      healthy = h.status === "ok";
      info = v;
    } catch {
      healthy = false;
    }
  });
</script>

<div class="min-h-screen flex flex-col">
  <header class="border-b border-neutral-800 px-6 py-4 flex items-center justify-between">
    <div class="flex items-center gap-2">
      <span class="text-lg font-semibold tracking-tight">Docklet</span>
      {#if info}
        <span class="text-xs text-neutral-500">{info.version}</span>
      {/if}
    </div>
    <div class="flex items-center gap-2 text-sm">
      <span
        class="h-2 w-2 rounded-full {healthy === null
          ? 'bg-neutral-500'
          : healthy
            ? 'bg-emerald-500'
            : 'bg-red-500'}"
      ></span>
      <span class="text-neutral-400">
        {healthy === null ? "Connecting…" : healthy ? "Control plane online" : "Unreachable"}
      </span>
    </div>
  </header>

  <main class="flex-1 grid place-items-center p-6">
    <div class="text-center max-w-md">
      <h1 class="text-2xl font-semibold mb-2">Your control plane is running</h1>
      <p class="text-neutral-400">
        Apps, databases and services will appear here. Setup and login arrive in M1.
      </p>
      {#if info}
        <p class="mt-6 text-xs text-neutral-600 font-mono">
          {info.commit} · {info.go} · {info.os}/{info.arch}
        </p>
      {/if}
    </div>
  </main>
</div>
