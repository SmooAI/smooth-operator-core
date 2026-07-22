#!/usr/bin/env node
/**
 * CI version script — runs during Changeset version-PR creation
 * (wired as the `version:` input of changesets/action in release.yml).
 *
 *   1. `changeset version` — consume changeset files, bump
 *      typescript/core/package.json (the canonical npm version).
 *   2. `version:sync`      — propagate that version to the Rust/.NET/Python/Go
 *      manifests so the SAME version PR carries every polyglot bump.
 *
 * Without step 2 in the version step, the sibling manifests would only be
 * rewritten at publish time — after the version PR already merged with stale
 * Cargo.toml / csproj / pyproject / version.go. Doing it here keeps the repo
 * self-consistent at every commit on main.
 */
import { execSync } from "node:child_process";
import process from "node:process";

const root = process.cwd();

function run(cmd) {
    console.log(`\n> ${cmd}`);
    execSync(cmd, { stdio: "inherit", cwd: root });
}

run("pnpm changeset version");
run("pnpm version:sync");
