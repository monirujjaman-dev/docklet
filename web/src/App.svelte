<script lang="ts">
  import { onMount } from "svelte";
  import { session } from "./lib/session.svelte";
  import { router, link, match } from "./lib/router.svelte";
  import Setup from "./pages/Setup.svelte";
  import Login from "./pages/Login.svelte";
  import Overview from "./pages/Overview.svelte";
  import Audit from "./pages/Audit.svelte";
  import Account from "./pages/Account.svelte";
  import Settings from "./pages/Settings.svelte";
  import Monitoring from "./pages/Monitoring.svelte";
  import Server from "./pages/Server.svelte";
  import Terminal from "./pages/Terminal.svelte";
  import Files from "./pages/Files.svelte";
  import ReauthModal from "./lib/ReauthModal.svelte";
  import NewApp from "./pages/NewApp.svelte";
  import AppDetail from "./pages/AppDetail.svelte";
  import NewDatabase from "./pages/NewDatabase.svelte";
  import DatabaseDetail from "./pages/DatabaseDetail.svelte";

  const nav = [
    { href: "/", label: "Projects" },
    { href: "/monitoring", label: "Monitoring" },
    { href: "/server", label: "Server" },
    { href: "/terminal", label: "Terminal" },
    { href: "/files", label: "Files" },
    { href: "/audit", label: "Audit log" },
    { href: "/settings", label: "Settings" },
    { href: "/account", label: "Account" },
  ];

  const active = (href: string) =>
    href === "/"
      ? router.path === "/" || router.path.startsWith("/apps") || router.path.startsWith("/projects") || router.path.startsWith("/databases")
      : router.path.startsWith(href);
  const newAppRoute = $derived(match("/projects/:id/new", router.path));
  const appRoute = $derived(match("/apps/:id", router.path));
  const newDbRoute = $derived(match("/projects/:id/new-database", router.path));
  const dbRoute = $derived(match("/databases/:id", router.path));

  onMount(() => session.init());
</script>

{#if session.phase === "loading"}
  <div class="min-h-screen grid place-items-center text-neutral-500 text-sm">Loading…</div>
{:else if session.phase === "error"}
  <div class="min-h-screen grid place-items-center p-6 text-center">
    <div>
      <p class="text-red-400 mb-3">Cannot reach the control plane: {session.error}</p>
      <button onclick={() => session.init()} class="rounded-md border border-neutral-700 px-3 py-1.5 text-sm">Retry</button>
    </div>
  </div>
{:else if session.phase === "setup"}
  <Setup />
{:else if session.phase === "login"}
  <Login />
{:else}
  <div class="min-h-screen flex">
    <aside class="w-52 shrink-0 border-r border-neutral-800 p-4 flex flex-col">
      <div class="text-lg font-semibold tracking-tight mb-6 px-2">Docklet</div>
      <nav class="space-y-1 flex-1">
        {#each nav as item}
          <a
            href={item.href}
            use:link
            class="block rounded-md px-2 py-1.5 text-sm {active(item.href)
              ? 'bg-neutral-800 text-white'
              : 'text-neutral-400 hover:text-white hover:bg-neutral-900'}">{item.label}</a
          >
        {/each}
      </nav>
      <div class="border-t border-neutral-800 pt-3 px-2 text-sm">
        <p class="text-neutral-300">{session.user?.username}</p>
        <button onclick={() => session.logout()} class="text-neutral-500 hover:text-white text-xs mt-1">Sign out</button>
      </div>
    </aside>
    <main class="flex-1 p-8 overflow-auto">
      {#if newDbRoute}
        <NewDatabase projectId={newDbRoute.id} />
      {:else if dbRoute}
        {#key dbRoute.id}<DatabaseDetail dbId={dbRoute.id} />{/key}
      {:else if newAppRoute}
        <NewApp projectId={newAppRoute.id} />
      {:else if appRoute}
        {#key appRoute.id}<AppDetail appId={appRoute.id} />{/key}
      {:else if router.path.startsWith("/monitoring")}
        <Monitoring />
      {:else if router.path.startsWith("/server")}
        <Server />
      {:else if router.path.startsWith("/terminal")}
        <Terminal />
      {:else if router.path.startsWith("/files")}
        <Files />
      {:else if router.path.startsWith("/audit")}
        <Audit />
      {:else if router.path.startsWith("/settings")}
        <Settings />
      {:else if router.path.startsWith("/account")}
        <Account />
      {:else}
        <Overview />
      {/if}
    </main>
  </div>
  <ReauthModal />
{/if}
