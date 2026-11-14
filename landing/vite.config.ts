import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

// GitHub project pages serve the site from /knock/.
// For a custom domain, build with KNOCK_BASE=/ and add public/CNAME.
export default defineConfig({
  base: process.env.KNOCK_BASE ?? "/knock/",
  plugins: [tailwindcss()],
  build: { target: "es2022" },
});
