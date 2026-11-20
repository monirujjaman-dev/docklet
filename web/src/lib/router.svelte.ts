// Tiny history-based router: enough for a handful of pages, no dependency.

class Router {
  path = $state(location.pathname);

  constructor() {
    window.addEventListener("popstate", () => (this.path = location.pathname));
  }

  go(path: string) {
    if (path === this.path) return;
    history.pushState({}, "", path);
    this.path = path;
  }
}

export const router = new Router();

/** Match a pattern like "/apps/:id" against a path; returns params or null. */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split("/").filter(Boolean);
  const a = path.split("/").filter(Boolean);
  if (p.length !== a.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(":")) params[p[i].slice(1)] = decodeURIComponent(a[i]);
    else if (p[i] !== a[i]) return null;
  }
  return params;
}

/** Svelte action: intercept same-origin link clicks. */
export function link(node: HTMLAnchorElement) {
  const onClick = (e: MouseEvent) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const href = node.getAttribute("href");
    if (!href || !href.startsWith("/")) return;
    e.preventDefault();
    router.go(href);
  };
  node.addEventListener("click", onClick);
  return { destroy: () => node.removeEventListener("click", onClick) };
}
