import { defineConfig, type Plugin } from "vite";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { mockBackend } from "./mock/plugin.ts";

const outDir = fileURLToPath(new URL("../internal/webui/dist", import.meta.url));

// The Go module embeds internal/webui/dist with //go:embed, which needs the
// directory to exist even before the first build. emptyOutDir wipes it, so
// put the placeholder back once the bundle is written.
function keepFile(): Plugin {
  return {
    name: "nsl-keep-file",
    apply: "build",
    closeBundle() {
      writeFileSync(`${outDir}/.keep`, "");
    },
  };
}

// `NSL_BACKEND=http://host:8080 npm run dev` serves this source against a
// real backend instead of the mock: /api and /listen (including the
// spectrum WebSocket) are proxied there.
const backend = process.env.NSL_BACKEND;

export default defineConfig({
  // The mock backend is a dev-server plugin only (apply: "serve"); nothing
  // under mock/ is imported by src/, so none of it reaches the bundle.
  plugins: [...(backend ? [] : [mockBackend()]), keepFile()],
  server: backend
    ? {
        proxy: {
          "/api": { target: backend, changeOrigin: true },
          "/listen": {
            target: backend,
            changeOrigin: true,
            ws: true,
            // The backend checks the socket's Origin against its own host.
            configure: (proxy) => {
              proxy.on("proxyReqWs", (req) => req.setHeader("Origin", backend));
            },
          },
        },
      }
    : undefined,
  build: {
    outDir,
    emptyOutDir: true,
    target: "es2022",
    assetsInlineLimit: 0,
  },
});
