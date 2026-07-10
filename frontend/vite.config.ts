import { defineConfig } from "vite";

export default defineConfig({
  server: {
    port: 5173,
    proxy: {
      "/ws": {
        target: "ws://localhost:8081",
        ws: true,
      },
      "/healthz": {
        target: "http://localhost:8081",
        changeOrigin: true,
      },
      // Proxy OTLP trace exports to motel so the browser makes same-origin
      // requests (motel doesn't send CORS headers). Only used in dev.
      "/v1/traces": {
        target: "http://127.0.0.1:27686",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "../dist/web",
    emptyOutDir: true,
  },
});
