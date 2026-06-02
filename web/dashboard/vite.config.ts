import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: "../../internal/dashboard/dist",
    emptyOutDir: true,
  },
});
