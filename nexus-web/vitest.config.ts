import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";

// Vitest config — maps the `@/*` path alias (tsconfig: `@/* → ./*`) so unit tests can VALUE-import
// project modules (e.g. `@/lib/api`), not just type-import them. Without this, value imports through
// the alias fail to resolve under Vite's module loader.
// Environment: happy-dom for React component tests; pure lib unit tests also run here.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: "happy-dom",
    globals: true,
    include: [
      "lib/**/*.test.ts",
      "components/**/*.test.{ts,tsx}",
      "test/**/*.test.{ts,tsx}",
    ],
    setupFiles: ["./test/setup.ts"],
    // Locally many worktrees run the suite at once (campaign workflows); a default of one worker
    // per core per run pushed load past 20 and stalled agents. CI keeps vitest's default.
    maxWorkers: process.env.VITEST_MAX_WORKERS ?? (process.env.CI ? undefined : 4),
  },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./", import.meta.url)),
    },
  },
});
