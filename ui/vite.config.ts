import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: { port: 5183, proxy: { "/api": { target: "http://127.0.0.1:6767" } } },
  build: { outDir: "dist", emptyOutDir: true, assetsInlineLimit: 0 },
});
