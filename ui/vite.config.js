import { defineConfig } from "vite";

// The UI is served by the app under the Home Assistant ingress path prefix,
// so every asset and API URL must be relative (base: "./").
//
// `mise run ui:dev` proxies /api to a local `hcs serve` (default :8099). Set
// HCS_TOKEN to the content of <state-dir>/api_token so the proxy can
// authenticate (ingress needs no token).
const target = process.env.HCS_API ?? "http://127.0.0.1:8099";
const token = process.env.HCS_TOKEN ?? "";

export default defineConfig({
  base: "./",
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
  },
  server: {
    proxy: {
      "/api": {
        target,
        changeOrigin: true,
        headers: token ? { Authorization: `Bearer ${token}` } : {},
      },
    },
  },
});
