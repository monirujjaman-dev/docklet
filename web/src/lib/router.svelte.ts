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
