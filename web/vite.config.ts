import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the Go backend runs separately; proxy API calls to it.
const backend = process.env.HLM_BACKEND ?? process.env.GSH_BACKEND ?? "http://localhost:8080";

// Listen on every interface so other computers on the network can open the
// dev server. Set HLM_DEV_HOST=localhost to keep it local.
const host = process.env.HLM_DEV_HOST ?? process.env.GSH_DEV_HOST ?? "0.0.0.0";

export default defineConfig({
  plugins: [react()],
  server: {
    host,
    port: 5173,
    // Vite rejects requests whose Host header isn't localhost or an IP, to
    // guard against DNS rebinding. Allow any name so the machine can be
    // reached by its hostname (e.g. http://my-server.lan:5173).
    allowedHosts: true,
    proxy: {
      "/api": { target: backend, changeOrigin: true },
    },
  },
  preview: {
    host,
    allowedHosts: true,
    proxy: {
      "/api": { target: backend, changeOrigin: true },
    },
  },
});
