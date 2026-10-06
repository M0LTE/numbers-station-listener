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

export default defineConfig({
  // The mock backend is a dev-server plugin only (apply: "serve"); nothing
  // under mock/ is imported by src/, so none of it reaches the bundle.
  plugins: [mockBackend(), keepFile()],
  build: {
    outDir,
    emptyOutDir: true,
    target: "es2022",
    assetsInlineLimit: 0,
  },
});
