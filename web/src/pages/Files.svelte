<script lang="ts">
  import { onMount } from "svelte";
  import { api, ApiError, getCsrfToken } from "../lib/api";
  import { bytes } from "../lib/format";
  import { needsReauth, reauth, withReauth } from "../lib/reauth.svelte";

  type Root = { id: string; label: string; path: string };
  type Entry = { name: string; type: "dir" | "file" | "link" | "other"; size: number; mode: string; owner: string; group: string; mtime: number; target?: string };
  type Opened = { path: string; text: string; size: number; sha256: string; mode: string };

  let roots = $state<Root[]>([]);
  let root = $state("host");
  let path = $state("/");
  let entries = $state<Entry[]>([]);
  let truncated = $state(false);
  let warning = $state("");
  let error = $state("");
  let msg = $state("");
  let busy = $state("");
  let file = $state<Opened | null>(null);
  let text = $state("");
  let fileWarning = $state("");
  let upload = $state<{ name: string; pct: number } | null>(null);

  async function run(label: string, fn: () => Promise<unknown>, done?: string) {
    error = msg = "";
    busy = label;
    try {
      await fn();
      if (done) msg = done;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      busy = "";
    }
  }

  const q = (p: Record<string, string>) => new URLSearchParams({ root, ...p }).toString();
  const join = (dir: string, name: string) => (dir === "/" ? "/" : dir + "/") + name;
  const crumbs = $derived(path.split("/").filter(Boolean).map((part, i, all) => ({ part, path: "/" + all.slice(0, i + 1).join("/") })));

  async function load(p = path) {
    await run("load", async () => {
      const r = await api<{ listing: { path: string; entries: Entry[]; truncated: boolean }; warning: string }>(`/files/list?${q({ path: p })}`);
      path = r.listing.path;
      entries = r.listing.entries;
      truncated = r.listing.truncated;
      warning = r.warning;
      history.replaceState({}, "", `/files?${q({ path })}`);
    });
  }

  function open(e: Entry) {
    const p = join(path, e.name);
    if (e.type === "dir") return load(p);
    return run("open", async () => {
      try {
        const r = await api<{ file: Opened; warning: string }>(`/files/read?${q({ path: p })}`);
        file = r.file;
        text = r.file.text;
        fileWarning = r.warning;
      } catch (err) {
        if (e.type === "link" && err instanceof ApiError && /not a regular file/.test(err.message)) return load(p);
        throw err;
      }
    });
  }

  const save = () =>
    run("save", async () => {
      if (!file) return;
      const r = await withReauth(() => api<{ sha256: string }>("/files/write", { method: "PUT", json: { root, path: file!.path, text, sha256: file!.sha256 } }));
      file = { ...file, sha256: r.sha256, text };
      await load();
    }, "Saved.");

  const op = (name: string, json: Record<string, unknown>, done: string) =>
    run(name, async () => {
      await withReauth(() => api(`/files/op/${name}`, { json: { root, ...json } }));
      await load();
    }, done);

  function newFolder() {
    const n = prompt("Folder name");
    if (n) op("mkdir", { path, name: n }, `Folder ${n} created.`);
  }
  function newFile() {
    const n = prompt("File name");
    if (!n) return;
    run("new", async () => {
      await withReauth(() => api("/files/write", { method: "PUT", json: { root, path: join(path, n), text: "", sha256: "" } }));
      await load();
      file = { path: join(path, n), text: "", size: 0, sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", mode: "0644" };
      text = "";
    });
  }
  function rename(e: Entry) {
    const n = prompt(`Rename ${e.name} to`, e.name);
    if (n && n !== e.name) op("rename", { path: join(path, e.name), name: n }, "Renamed.");
  }
  function move(e: Entry) {
    const d = prompt(`Move ${e.name} to folder`, path);
    if (d && d !== path) op("move", { path: join(path, e.name), dest: d }, "Moved.");
  }
  function del(e: Entry) {
    if (confirm(`Delete ${join(path, e.name)}${e.type === "dir" ? " and everything in it" : ""}? This can't be undone.`))
      op("delete", { path: join(path, e.name) }, "Deleted.");
  }
  function chmod(e: Entry) {
    const m = prompt(`Mode for ${e.name} (octal)`, e.mode);
    if (!m || m === e.mode) return;
    const recursive = e.type === "dir" && confirm("Apply to everything inside the folder too?");
    op("chmod", { path: join(path, e.name), mode: m, recursive }, "Mode changed.");
  }
  function chown(e: Entry) {
    const v = prompt(`Owner for ${e.name} (user or user:group)`, `${e.owner}:${e.group}`);
    if (!v || v === `${e.owner}:${e.group}`) return;
    const [owner, group = ""] = v.split(":");
    const recursive = e.type === "dir" && confirm("Apply to everything inside the folder too?");
    op("chown", { path: join(path, e.name), owner, group, recursive }, "Owner changed.");
  }
  const downloadUrl = (e: Entry) => `/api/files/download?${q({ path: join(path, e.name) })}`;

  // Uploads use XHR for progress; the body is the raw file.
  function send(f: File, overwrite: boolean): Promise<void> {
    return new Promise((ok, fail) => {
      const x = new XMLHttpRequest();
      x.open("POST", `/api/files/upload?${q({ dir: path, name: f.name, overwrite: overwrite ? "1" : "0" })}`);
      x.setRequestHeader("X-CSRF-Token", getCsrfToken());
      x.setRequestHeader("Content-Type", "application/octet-stream");
      x.upload.onprogress = (ev) => upload && (upload.pct = Math.round((ev.loaded / ev.total) * 100));
      x.onload = () => {
        let body: Record<string, unknown> = {};
        try {
          body = JSON.parse(x.responseText);
        } catch {
          /* not JSON */
        }
        if (x.status < 300) ok();
        else fail(new ApiError(x.status, (body.error as string) ?? x.statusText, body));
      };
      x.onerror = () => fail(new Error("upload failed (network)"));
      x.send(f);
    });
  }
  async function pick(ev: Event) {
    const input = ev.currentTarget as HTMLInputElement;
    const files = [...(input.files ?? [])];
    input.value = "";
    for (const f of files) {
      if (f.size > 1 << 30) {
        error = `${f.name} is larger than 1 GB.`;
        continue;
      }
      const exists = entries.some((e) => e.name === f.name);
      if (exists && !confirm(`${f.name} exists. Replace it?`)) continue;
      upload = { name: f.name, pct: 0 };
      await run("upload", async () => {
        try {
          await send(f, exists);
        } catch (e) {
          if (!needsReauth(e)) throw e;
          await reauth.ask();
          await send(f, exists);
        }
      }, `Uploaded ${f.name}.`);
      upload = null;
    }
    await load();
  }

  onMount(async () => {
    const p = new URLSearchParams(location.search);
    if (p.get("root")) root = p.get("root")!;
    try {
      roots = (await api<{ roots: Root[] }>("/files/roots")).roots;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
    await load(p.get("path") ?? "/");
  });

  const when = (t: number) => new Date(t * 1000).toLocaleString();
  const btn = "rounded-md border border-neutral-700 hover:border-neutral-500 px-3 py-1.5 text-sm disabled:opacity-50";
</script>

<div class="flex flex-wrap items-center gap-3 mb-4">
  <h1 class="text-xl font-semibold mr-2">Files</h1>
  <select bind:value={root} onchange={() => { file = null; load("/"); }} class="rounded-md bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-sm max-w-md">
    {#each roots as r}<option value={r.id}>{r.label}</option>{/each}
  </select>
  <button onclick={newFolder} disabled={!!busy} class={btn}>New folder</button>
  <button onclick={newFile} disabled={!!busy} class={btn}>New file</button>
  <label class="{btn} cursor-pointer">Upload<input type="file" multiple onchange={pick} class="hidden" /></label>
  {#if upload}<span class="text-sm text-neutral-400">Uploading {upload.name}: {upload.pct}%</span>{/if}
</div>

<nav class="text-sm mb-2 font-mono flex flex-wrap gap-1">
  <button onclick={() => load("/")} class="text-emerald-300 hover:underline">/</button>
  {#each crumbs as c}<span class="text-neutral-600">›</span><button onclick={() => load(c.path)} class="text-emerald-300 hover:underline">{c.part}</button>{/each}
</nav>
{#if warning}<p class="text-sm text-amber-300 mb-2">{warning}</p>{/if}
{#if error}<p class="text-sm text-red-400 mb-2">{error}</p>{/if}
{#if msg}<p class="text-sm text-emerald-300 mb-2">{msg}</p>{/if}

{#if file}
  <section class="rounded-lg border border-neutral-800 p-4 mb-4 space-y-2">
    <div class="flex items-center justify-between gap-2">
      <p class="text-sm font-mono">{file.path} <span class="text-neutral-500">· {bytes(file.size)} · {file.mode}</span></p>
      <div class="flex gap-2">
        <button onclick={save} disabled={!!busy || text === file.text} class="rounded-md bg-emerald-600 hover:bg-emerald-500 disabled:opacity-50 px-3 py-1.5 text-sm font-medium">{busy === "save" ? "Saving…" : "Save"}</button>
        <button onclick={() => (text === file?.text || confirm("Discard your changes?")) && (file = null)} class={btn}>Close</button>
      </div>
    </div>
    {#if fileWarning}<p class="text-xs text-amber-300">{fileWarning} Saving keeps the file's mode and owner and fails if someone changed it meanwhile.</p>{/if}
    <textarea bind:value={text} spellcheck="false" class="w-full h-[50vh] rounded-md bg-neutral-950 border border-neutral-800 p-3 font-mono text-xs outline-none focus:border-emerald-600"></textarea>
  </section>
{/if}

<div class="rounded-lg border border-neutral-800 overflow-x-auto">
  <table class="w-full text-sm">
    <thead class="text-left text-xs text-neutral-400 border-b border-neutral-800">
      <tr><th class="px-3 py-2">Name</th><th class="px-3">Size</th><th class="px-3">Mode</th><th class="px-3">Owner</th><th class="px-3">Modified</th><th></th></tr>
    </thead>
    <tbody class="divide-y divide-neutral-900">
      {#if path !== "/"}
        <tr><td class="px-3 py-1.5" colspan="6"><button onclick={() => load(path.replace(/\/[^/]+$/, "") || "/")} class="text-neutral-300 hover:underline">..</button></td></tr>
      {/if}
      {#each entries as e (e.name)}
        <tr class="hover:bg-neutral-900/50">
          <td class="px-3 py-1.5 font-mono text-xs">
            <button onclick={() => open(e)} class="hover:underline text-left {e.type === 'dir' ? 'text-emerald-300' : e.type === 'link' ? 'text-sky-300' : 'text-neutral-200'}">{e.name}{e.type === "dir" ? "/" : ""}</button>
            {#if e.target}<span class="text-neutral-500"> → {e.target}</span>{/if}
          </td>
          <td class="px-3 text-neutral-400 whitespace-nowrap">{e.type === "file" ? bytes(e.size) : ""}</td>
          <td class="px-3 font-mono text-xs text-neutral-400">{e.mode}</td>
          <td class="px-3 text-neutral-400 whitespace-nowrap">{e.owner}:{e.group}</td>
          <td class="px-3 text-neutral-500 whitespace-nowrap text-xs">{when(e.mtime)}</td>
          <td class="px-3 text-right whitespace-nowrap space-x-2 text-xs">
            {#if e.type === "file" || e.type === "dir"}<a href={downloadUrl(e)} class="text-neutral-300 hover:underline" download>Download</a>{/if}
            <button onclick={() => rename(e)} class="text-neutral-300 hover:underline">Rename</button>
            <button onclick={() => move(e)} class="text-neutral-300 hover:underline">Move</button>
            <button onclick={() => chmod(e)} class="text-neutral-400 hover:underline">Mode</button>
            <button onclick={() => chown(e)} class="text-neutral-400 hover:underline">Owner</button>
            <button onclick={() => del(e)} class="text-red-300 hover:underline">Delete</button>
          </td>
        </tr>
      {:else}
        <tr><td colspan="6" class="px-3 py-3 text-neutral-500">{busy === "load" ? "Loading…" : "Empty folder."}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
{#if truncated}<p class="text-xs text-amber-300 mt-2">Showing the first 5000 entries.</p>{/if}
<p class="text-xs text-neutral-500 mt-3">Files open in the editor up to 1 MB (UTF-8 text). Uploads up to 1 GB. Folders download as .tar. Every change and download is recorded in the audit log.</p>
