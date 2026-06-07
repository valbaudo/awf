import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// base "./" -> assets referenced relatively so the bundle works when served from
// the Go server's embedded FS at "/". outDir "dist" is what ui/embed.go embeds.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: { outDir: "dist", emptyOutDir: true },
  test: { environment: "node" },
});
