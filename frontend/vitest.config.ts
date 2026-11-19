import { defineConfig, mergeConfig } from "vitest/config";
import viteConfig from "./vite.config";

// The behaviour tests reuse the exact build aliases (original app/ components,
// React single copy, shims) so they exercise what actually ships.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      environment: "jsdom",
      include: ["src/tests/**/*.test.tsx"],
      environmentOptions: { jsdom: { url: "http://localhost/" } },
      restoreMocks: true,
    },
  }),
);
