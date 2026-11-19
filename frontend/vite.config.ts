import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { defineConfig, type Plugin } from "vite";

const frontendDir = path.dirname(fileURLToPath(import.meta.url));
const thirdPartyLicenses = readFileSync(
  path.join(frontendDir, "THIRD_PARTY_LICENSES.txt"),
  "utf8",
).trim();
if (!thirdPartyLicenses || thirdPartyLicenses.includes("*/")) {
  throw new Error("THIRD_PARTY_LICENSES.txt must contain valid license text for the bundle banner");
}

// The reused browser components live inside this package (frontend/app,
// frontend/lib) so the frontend can be built from a clean copy of this
// directory alone: "@/..." resolves here, never to a parent directory, and
// the Next-only module specifiers come from the shims in src/shims/.
const nodeModules = path.join(frontendDir, "node_modules");

const reactAliases = [
  // Exactly one React copy for the frontend sources and the reused app/
  // sources: the aliases keep resolution inside this package so the bundle
  // never picks up a second, incompatible React instance.
  { find: /^react$/, replacement: path.join(nodeModules, "react") },
  { find: /^react\/jsx-runtime$/, replacement: path.join(nodeModules, "react/jsx-runtime.js") },
  { find: /^react\/jsx-dev-runtime$/, replacement: path.join(nodeModules, "react/jsx-dev-runtime.js") },
  { find: /^react-dom$/, replacement: path.join(nodeModules, "react-dom") },
  { find: /^react-dom\/client$/, replacement: path.join(nodeModules, "react-dom/client.js") },
];

// The reused components start with "use client" for the Next.js compiler.
// Rollup only warns about it, but stripping the directive keeps the build log
// honest about what actually ships (a plain browser module).
function stripUseClientDirective(): Plugin {
  return {
    name: "picbox-strip-use-client",
    enforce: "pre",
    transform(code, id) {
      if (!id.includes("/app/")) return null;
      const match = /^\s*["']use client["'];?[ \t]*\r?\n/.exec(code);
      if (!match) return null;
      return { code: code.slice(match[0].length), map: null };
    },
  };
}

function readVersion(): string {
  const packageJson = JSON.parse(
    readFileSync(path.join(frontendDir, "package.json"), "utf8"),
  ) as { version?: string };
  return String(packageJson.version ?? "unknown");
}

export default defineConfig({
  root: frontendDir,
  // The reused app/ files are plain browser components, not Next.js pages, so
  // the JSX runtime is pinned here instead of being inferred from a tsconfig.
  // A clean frontend-only install therefore produces byte-identical output.
  esbuild: { jsx: "automatic", jsxImportSource: "react", legalComments: "inline" },
  // The Go binary serves dist/ itself. Emitted JS/CSS land in dist/assets/,
  // which is the only asset path the Go embed handler exposes.
  base: "/",
  publicDir: false,
  build: {
    outDir: path.join(frontendDir, "dist"),
    emptyOutDir: true,
    sourcemap: false,
    // Keep the runtime dependencies' notices in every delivered JS copy,
    // including assets served directly from the embedded Go filesystem.
    rollupOptions: {
      output: { banner: `/*!\n${thirdPartyLicenses}\n*/` },
    },
    assetsDir: "assets",
    // Every asset must stay a real file: data: URLs are blocked by the Go
    // handler's Content-Security-Policy (img-src 'self').
    assetsInlineLimit: 0,
    // Vite's modulePreload polyfill is an inline <script>; the Go handler's
    // Content-Security-Policy only allows script-src 'self'.
    modulePreload: { polyfill: false },
  },
  define: {
    __PICBOX_VERSION__: JSON.stringify(readVersion()),
    // DashboardClient reads this build-time constant. The Go binary owns the
    // real upload ceiling (UPLOAD_MAX_BYTES); set the same value here at build
    // time so the UI hint matches the server limit.
    "process.env.NEXT_PUBLIC_UPLOAD_MAX_BYTES": JSON.stringify(
      process.env.NEXT_PUBLIC_UPLOAD_MAX_BYTES ?? "",
    ),
  },
  plugins: [stripUseClientDirective()],
  resolve: {
    dedupe: ["react", "react-dom"],
    alias: [
      ...reactAliases,
      { find: "next/navigation", replacement: path.join(frontendDir, "src/shims/next-navigation.ts") },
      // The reused components import the generated Prisma enum, which is not
      // part of this repository; a three-value shim supplies both role strings.
      { find: "@/app/generated/prisma/enums", replacement: path.join(frontendDir, "src/shims/prisma-enums.ts") },
      { find: "@/", replacement: `${frontendDir}/` },
    ],
  },
});
