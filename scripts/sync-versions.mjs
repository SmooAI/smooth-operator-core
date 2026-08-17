#!/usr/bin/env node
/**
 * Sync the canonical version → every publishable polyglot manifest.
 *
 * The single source of truth is the npm package `@smooai/smooth-operator-core`
 * (typescript/core/package.json) — the one package Changesets version-bumps.
 * This script propagates that version, in lockstep, to the sibling engines so a
 * changeset release ships npm + crates.io + NuGet + PyPI + Go all at one number:
 *
 *   1. Rust   — rust/smooth-operator-core/Cargo.toml   [package] version
 *   2. .NET   — dotnet/core/src/SmooAI.SmoothOperator.Core.csproj  <Version>
 *   3. Python — python/core/pyproject.toml             [project] version
 *   4. Go     — go/version.go                          const Version (the anchor
 *               scripts read for the `go/vX.Y.Z` publish tag; see ci-publish.mjs)
 *
 * NOT touched, on purpose:
 *   • Cargo.lock — gitignored in this repo (see .gitignore), so there is nothing
 *     tracked to rewrite. `cargo publish` regenerates it.
 *   • rust/smooth-operator-temporal's dependency on core — pinned at `^1.8`, not
 *     the exact lockstep version, so it stays satisfiable across patch releases
 *     without an anchor of its own.
 *
 * The temporal crate's own [package] version IS synced (anchor below): it used to
 * be `publish = false` and therefore inert, but it now publishes to crates.io, so
 * a drifting version would make ci-publish's `cratesHasVersion` check never
 * match — it would try to publish the stale version forever.
 *
 * FAILS LOUDLY: if any expected version anchor is missing we throw rather than
 * silently ship a mismatched set — a partial sync must abort the release, never
 * publish half the languages at the new version and half at the old one.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import process from "node:process";

const root = process.cwd();

// Canonical version = the npm package Changesets bumps.
const canonicalPkgPath = resolve(root, "typescript/core/package.json");
const canonicalPkg = JSON.parse(readFileSync(canonicalPkgPath, "utf8"));
const version = canonicalPkg.version;

if (!version) {
    console.error("Unable to read version from typescript/core/package.json");
    process.exit(1);
}
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
    console.error(`Refusing to sync a non-semver version: "${version}"`);
    process.exit(1);
}

/**
 * Each anchor pins a version inside exactly one manifest. `pattern` MUST have
 * two capture groups wrapping the version text so we can splice the new value
 * in without disturbing surrounding formatting.
 */
const anchors = [
    {
        label: "Rust crate (Cargo.toml [package] version)",
        path: "rust/smooth-operator-core/Cargo.toml",
        pattern: /(name = "smooai-smooth-operator-core"\nversion = ")[^"]+(")/,
    },
    {
        label: "Rust temporal crate (Cargo.toml [package] version)",
        path: "rust/smooth-operator-temporal/Cargo.toml",
        pattern: /(name = "smooai-smooth-operator-temporal"\nversion = ")[^"]+(")/,
    },
    {
        label: ".NET package (csproj <Version>)",
        path: "dotnet/core/src/SmooAI.SmoothOperator.Core.csproj",
        pattern: /(<Version>)[^<]+(<\/Version>)/,
    },
    {
        label: "Python package (pyproject [project] version)",
        path: "python/core/pyproject.toml",
        pattern: /(name = "smooai-smooth-operator-core"\nversion = ")[^"]+(")/,
    },
    {
        label: "Go module (version.go const Version)",
        path: "go/version.go",
        pattern: /(const Version = ")[^"]+(")/,
    },
    {
        // uv.lock records the local editable package's version and `uv sync
        // --locked` (the first step of py-checks) HARD-FAILS when it drifts from
        // pyproject. Every version bump that skipped this line turned py-checks
        // red on main until someone hand-refreshed the lock — syncing it here
        // ends that treadmill.
        label: "Python lockfile (uv.lock local package version)",
        path: "python/core/uv.lock",
        pattern: /(name = "smooai-smooth-operator-core"\nversion = ")[^"]+(")/,
    },
];

let touched = 0;

for (const { label, path, pattern } of anchors) {
    const absolutePath = resolve(root, path);
    let content;
    try {
        content = readFileSync(absolutePath, "utf8");
    } catch (error) {
        if (error && error.code === "ENOENT") {
            throw new Error(`Version anchor file missing: ${path} (${label})`);
        }
        throw error;
    }

    if (!pattern.test(content)) {
        throw new Error(`Version anchor not found in ${path} (${label}) — refusing partial sync`);
    }

    const next = content.replace(pattern, `$1${version}$2`);
    if (next !== content) {
        writeFileSync(absolutePath, next);
        touched += 1;
        console.log(`Updated ${label} → ${version} (${path})`);
    } else {
        console.log(`Already ${version}: ${label} (${path})`);
    }
}

console.log(`\nSynced version ${version} across ${anchors.length} manifest(s); ${touched} changed.`);
