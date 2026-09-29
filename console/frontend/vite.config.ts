import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "/assets/",
  build: {
    outDir: resolve(
      fileURLToPath(new URL("../backend/assets", import.meta.url)),
    ),
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: "console.js",
        assetFileNames: "console.[ext]",
      },
    },
  },
});
