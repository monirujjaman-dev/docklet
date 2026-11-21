<script lang="ts">
  let { status }: { status: string } = $props();
  const styles: Record<string, string> = {
    running: "bg-emerald-950 text-emerald-300",
    succeeded: "bg-emerald-950 text-emerald-300",
    queued: "bg-neutral-800 text-neutral-300",
    pending: "bg-amber-950 text-amber-300",
    restarting: "bg-amber-950 text-amber-300",
    stopped: "bg-neutral-800 text-neutral-400",
    superseded: "bg-neutral-900 text-neutral-500",
    not_deployed: "bg-neutral-800 text-neutral-400",
    unknown: "bg-neutral-800 text-neutral-400",
    exited: "bg-red-950 text-red-300",
    missing: "bg-red-950 text-red-300",
    failed: "bg-red-950 text-red-300",
    timeout: "bg-red-950 text-red-300",
    skipped: "bg-neutral-900 text-neutral-400",
  };
  const label = $derived(status.replace("_", " "));
</script>

<span class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs {styles[status] ?? styles.unknown}">
  {#if status === "running" || status === "restarting"}
    <span class="h-1.5 w-1.5 rounded-full {status === 'running' ? 'bg-emerald-400' : 'bg-amber-400 animate-pulse'}"></span>
  {/if}
  {label}
</span>
