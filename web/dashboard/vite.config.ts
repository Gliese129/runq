import { fileURLToPath, URL } from "node:url";
import vue from "@vitejs/plugin-vue";
import vuetify from "vite-plugin-vuetify";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [
    vue(),
    vuetify({ autoImport: true }),
  ],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  server: {
    proxy: {
      "/api": "http://localhost:8077",
      "/ws": {
        target: "ws://localhost:8077",
        ws: true,
      },
    },
    hmr: true,
  },
  build: {
    outDir: "../../internal/dashboard/dist",
    emptyOutDir: true,
  },
});
