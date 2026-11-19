#!/usr/bin/env node
/**
 * Guardrails for the static bundle. The Go binary embeds dist/ and serves it
 * with a strict Content-Security-Policy, so a regression here fails at runtime
 * in the browser, not at build time. This script keeps the cheap invariants
 * checkable offline:
 *
 *  1. dist/index.html has exactly one script tag, and it is an external
 *     module under /assets/ (no inline script, which CSP script-src 'self'
 *     would block).
 *  2. Every referenced asset exists, stays flat under dist/assets/ (the Go
 *     handler serves GET /assets/{name} with one path segment) and uses an
 *     allowed extension.
 *  3. The bundle contains no server-only leftovers: process.env, Next.js
 *     module specifiers, Prisma, or Node builtin imports.
 *  4. Every JavaScript asset retains the runtime dependencies' license banner.
 */
import { readFileSync, existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const frontendDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const distDir = path.join(frontendDir, "dist");
const indexPath = path.join(distDir, "index.html");
const licenseText = readFileSync(path.join(frontendDir, "THIRD_PARTY_LICENSES.txt"), "utf8").trim();
if (!licenseText) {
  console.error("check-dist: THIRD_PARTY_LICENSES.txt is empty");
  process.exit(1);
}

const problems = [];
function fail(message) {
  problems.push(message);
}

if (!existsSync(indexPath)) {
  console.error("check-dist: dist/index.html is missing; run `vite build` first");
  process.exit(1);
}

const html = readFileSync(indexPath, "utf8");

const scriptTags = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)];
if (scriptTags.length !== 1) {
  fail(`expected exactly one <script> in index.html, found ${scriptTags.length}`);
}
for (const tag of scriptTags) {
  if (tag[1].trim() !== "") fail("inline script body found in index.html");
  if (!/\bsrc="([^"]+)"/.test(tag[0])) fail("script tag without src in index.html");
  if (!/type="module"/.test(tag[0])) fail("script tag is not a module in index.html");
}

const assetPaths = [...html.matchAll(/(?:src|href)="([^"]+)"/g)]
  .map((match) => match[1])
  .filter((url) => url.startsWith("/assets/") || url.startsWith("/src/"));

if (assetPaths.length === 0) fail("index.html references no assets");

for (const url of assetPaths) {
  const relative = url.replace(/^\//, "");
  if (relative.split("/").length !== 2) {
    fail(`asset must be flat under dist/assets/: ${url}`);
    continue;
  }
  const extension = path.extname(relative).toLowerCase();
  if (![".css", ".js", ".svg"].includes(extension)) {
    fail(`asset extension not served by the Go handler: ${url}`);
  }
  if (!existsSync(path.join(distDir, relative))) {
    fail(`referenced asset missing from dist: ${url}`);
  }
}

const jsFiles = assetPaths.filter((url) => url.endsWith(".js"));
if (jsFiles.length === 0) fail("no JavaScript bundle referenced from index.html");

const forbidden = [
  { pattern: "process.env", reason: "build-time process.env was not replaced" },
  { pattern: "NEXT_PUBLIC_", reason: "unreplaced NEXT_PUBLIC_* reference" },
  { pattern: '"next/navigation"', reason: "Next.js module reference left in the bundle" },
  { pattern: "PrismaClient", reason: "Prisma client pulled into the browser bundle" },
  { pattern: "from \"fs\"", reason: "Node builtin left in the bundle" },
];

for (const url of jsFiles) {
  const source = readFileSync(path.join(distDir, url.replace(/^\//, "")), "utf8");
  if (!source.startsWith(`/*!\n${licenseText}\n*/`)) {
    fail(`${url}: runtime dependency license banner is missing or outdated`);
  }
  for (const { pattern, reason } of forbidden) {
    if (source.includes(pattern)) fail(`${url}: ${reason} (${pattern})`);
  }
}

if (problems.length > 0) {
  console.error("check-dist: FAILED");
  for (const problem of problems) console.error(` - ${problem}`);
  process.exit(1);
}

console.log("check-dist: ok (single external module script, flat /assets files, no server-only code, license banner retained)");
