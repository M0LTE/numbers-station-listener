import { defineConfig, type Plugin, type UserConfig } from "vite";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

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

export default defineConfig(async ({ command }): Promise<UserConfig> => ({
  // The mock backend is for the dev server only. It is imported lazily so a
  // production build never loads it (it reads test fixtures at import time),
  // and nothing under mock/ is imported by src/, so none of it reaches the
  // bundle.
  plugins: [
    ...(command === "serve" && !backend ? [(await import("./mock/plugin.ts")).mockBackend()] : []),
    keepFile(),
  ],
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
}));
