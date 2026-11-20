<script lang="ts">
  let {
    label,
    value = $bindable(""),
    type = "text",
    autocomplete = "off",
    hint = "",
    inputmode,
  }: {
    label: string;
    value?: string;
    type?: string;
    autocomplete?: AutoFill;
    hint?: string;
    inputmode?: "numeric" | "text";
  } = $props();
  const id = `f-${Math.random().toString(36).slice(2, 8)}`;
</script>

<label for={id} class="block">
  <span class="block text-sm text-neutral-300 mb-1">{label}</span>
  <input
    {id}
    {type}
    {autocomplete}
    {inputmode}
    bind:value
    class="w-full rounded-md bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm outline-none focus:border-emerald-500 focus:ring-1 focus:ring-emerald-500"
  />
  {#if hint}<span class="block text-xs text-neutral-500 mt-1">{hint}</span>{/if}
</label>
