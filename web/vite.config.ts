import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import { writeFileSync } from "node:fs";
import { resolve } from "node:path";

// emptyOutDir wipes internal/server/dist; restore the placeholder that keeps
// the Go embed pattern valid in a fresh checkout.
const keepPlaceholder = {
  name: "keep-gitkeep",
  closeBundle() {
    writeFileSync(resolve(__dirname, "../internal/server/dist/.gitkeep"), "");
  },
};

// The build output is embedded into the Go binary (internal/server/dist).
// In dev, Vite proxies /api to a locally running `docklet serve`.
export default defineConfig({
  plugins: [svelte(), tailwindcss(), keepPlaceholder],
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
