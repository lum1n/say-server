import { tanstackStart } from "@tanstack/react-start/plugin/vite";
import viteReact from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// SPA mode: the static build is embedded into the Go binary, which serves the
// API. No SSR or server functions, so no Node runtime next to the binary.
export default defineConfig({
  server: {
    proxy: {
      "/api": { target: "http://localhost:7290", ws: true },
      "/widget.js": "http://localhost:7290",
    },
  },
  plugins: [tanstackStart({ spa: { enabled: true } }), viteReact()],
});
