// Ported from CXC v0.2.40 plugins/codexclaw/gui/vite.config.ts (1-11), modified: the
// dev-server API middleware is not ported (the CRW API is Go, internal/gui), so the
// middleware plugin import and the server block are gone and only the react plugin
// and the build output directory remain.
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // The built screens are committed under internal/gui/assets and embedded in the crw binary
  // with `//go:embed all:assets`, so a Node-free source checkout still serves a screen. emptyOutDir
  // removes a stale hashed asset a previous build left behind; the drift check compares the whole
  // tree, so a leftover would be refused rather than served.
  build: { outDir: "../internal/gui/assets", emptyOutDir: true },
});
