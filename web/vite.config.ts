import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

// The build output is embedded into the Go binary (internal/server/dist).
// In dev, Vite proxies /api to a locally running `docklet serve`.
export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  build: {
    outDir: "../internal/server/dist",
    emptyOutDir: true,
    assetsDir: "assets",
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://127.0.0.1:3000", changeOrigin: false, ws: true },
    },
  },
});
