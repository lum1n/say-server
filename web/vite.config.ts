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
  // The build prerenders the SPA shell from a preview server. Bind it to
  // 127.0.0.1: in a container "localhost" can resolve to an address the
  // server does not listen on.
  preview: { host: "127.0.0.1" },
  plugins: [tanstackStart({ spa: { enabled: true } }), viteReact()],
});
