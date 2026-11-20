<script lang="ts">
  import { onMount } from "svelte";
  import { api, type VersionInfo } from "../lib/api";

  let info = $state<VersionInfo | null>(null);
  onMount(async () => {
    info = await api<VersionInfo>("/version");
  });
</script>

<h1 class="text-xl font-semibold mb-2">Overview</h1>
<p class="text-neutral-400 text-sm">Apps, databases and services will appear here from M3 onward.</p>
{#if info}
  <p class="mt-6 text-xs text-neutral-600 font-mono">
    {info.version} · {info.commit} · {info.go} · {info.os}/{info.arch}
  </p>
{/if}
