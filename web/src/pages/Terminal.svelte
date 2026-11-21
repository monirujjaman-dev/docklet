<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { api, type Project } from "../lib/api";
  import { withReauth } from "../lib/reauth.svelte";
  import type { Terminal as XTerm } from "@xterm/xterm";

  type Target = { kind: "host" | "app" | "database"; id?: number; label: string };

  let targets = $state<Target[]>([{ kind: "host", label: "Server (host shell)" }]);
  let sel = $state("host");
  let status = $state<"idle" | "connecting" | "open" | "closed">("idle");
  let note = $state("");
  let error = $state("");
  let el: HTMLDivElement;
  let term: XTerm | null = null;
  let fit: { fit(): void } | null = null;
  let sock: WebSocket | null = null;
  let ro: ResizeObserver | null = null;

  onMount(async () => {
    const q = new URLSearchParams(location.search);
    if (q.get("kind")) sel = q.get("kind") === "host" ? "host" : `${q.get("kind")}:${q.get("id")}`;
    try {
      const { projects } = await api<{ projects: Project[] }>("/projects");
      for (const p of projects) {
        for (const a of p.apps) targets.push({ kind: "app", id: a.id, label: `${p.name} / ${a.name} (app)` });
        for (const d of p.databases) targets.push({ kind: "database", id: d.id, label: `${p.name} / ${d.name} (${d.engine})` });
      }
    } catch {
      /* host only */
    }
    if (q.get("kind")) connect();
  });

  async function connect() {
    disconnect();
    error = note = "";
    status = "connecting";
    const t = targets.find((x) => (x.kind === "host" ? "host" : `${x.kind}:${x.id}`) === sel) ?? targets[0];
    try {
      const [{ Terminal }, { FitAddon }] = await Promise.all([import("@xterm/xterm"), import("@xterm/addon-fit"), import("@xterm/xterm/css/xterm.css")]);
      const { ticket, label } = await withReauth(() => api<{ ticket: string; label: string }>("/terminal/sessions", { json: { kind: t.kind, id: t.id } }));
      term = new Terminal({ cursorBlink: true, fontSize: 13, fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", theme: { background: "#0a0a0a" }, scrollback: 5000 });
      const f = new FitAddon();
      fit = f;
      term.loadAddon(f);
      term.open(el);
      f.fit();
      const proto = location.protocol === "https:" ? "wss" : "ws";
      sock = new WebSocket(`${proto}://${location.host}/api/terminal/ws?ticket=${ticket}&rows=${term.rows}&cols=${term.cols}`);
      sock.binaryType = "arraybuffer";
      const enc = new TextEncoder();
      sock.onopen = () => {
        status = "open";
        note = `Connected to ${label}.`;
        term?.focus();
      };
      sock.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            const m = JSON.parse(ev.data);
            if (m.type === "error") error = m.error;
          } catch {
            /* ignore */
          }
          return;
        }
        term?.write(new Uint8Array(ev.data as ArrayBuffer));
      };
      sock.onclose = () => {
        status = "closed";
        term?.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
      };
      term.onData((d) => sock?.readyState === WebSocket.OPEN && sock.send(enc.encode(d)));
      term.onResize(({ rows, cols }) => sock?.readyState === WebSocket.OPEN && sock.send(JSON.stringify({ type: "resize", rows, cols })));
      ro = new ResizeObserver(() => fit?.fit());
      ro.observe(el);
    } catch (e) {
      status = "idle";
      error = e instanceof Error ? e.message : String(e);
    }
  }

  function disconnect() {
    ro?.disconnect();
    ro = null;
    sock?.close();
    sock = null;
    term?.dispose();
    term = null;
  }
  onDestroy(disconnect);
</script>

<div class="flex flex-col h-[calc(100vh-4rem)]">
  <div class="flex flex-wrap items-center gap-3 mb-3">
    <h1 class="text-xl font-semibold mr-2">Terminal</h1>
    <select bind:value={sel} class="rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm">
      {#each targets as t}<option value={t.kind === "host" ? "host" : `${t.kind}:${t.id}`}>{t.label}</option>{/each}
    </select>
    <button onclick={connect} disabled={status === "connecting"} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">
      {status === "open" ? "Reconnect" : status === "connecting" ? "Connecting…" : "Connect"}
    </button>
    {#if status === "open"}<button onclick={disconnect} class="rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm">Disconnect</button>{/if}
    <span class="text-sm text-neutral-400">{note}</span>
  </div>
  {#if error}<p class="text-sm text-red-400 mb-2">{error}</p>{/if}
  {#if sel === "host"}<p class="text-xs text-amber-300/80 mb-2">A root shell on the server. Closing this page ends the shell and everything started in it.</p>{/if}
  <div bind:this={el} class="flex-1 min-h-64 rounded-md border border-neutral-800 bg-[#0a0a0a] p-1 overflow-hidden"></div>
  <p class="text-xs text-neutral-500 mt-2">Copy: select text. Paste: Ctrl/Cmd+V. Idle sessions close after 30 minutes. Starting and ending a session is recorded in the audit log; what you type is not.</p>
</div>
