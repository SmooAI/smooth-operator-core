#!/usr/bin/env node
/**
 * CI publish orchestrator — runs after the Changeset version PR merges
 * (wired as the `publish:` input of changesets/action in release.yml).
 *
 * Publishes the ONE lockstep version to every polyglot registry:
 *
 *   • npm      — @smooai/smooth-operator-core          (typescript/core)
 *   • crates.io— smooai-smooth-operator-core           (rust/smooth-operator-core)
 *   • NuGet    — SmooAI.SmoothOperator.Core            (dotnet/core)
 *   • PyPI     — smooai-smooth-operator-core           (python/core)
 *   • Go       — git tag go/vX.Y.Z                     (go/  — "publish" == tag)
 *
 * ── SAFETY (these are IRREVERSIBLE registries — a NuGet/PyPI/crates version can
 *    never be deleted or reused) ────────────────────────────────────────────────
 *   • IDEMPOTENT: every registry is queried for the target version first and
 *     SKIPPED if already present, so a re-run never double-publishes and never
 *     hard-fails on an existing version. Belt: --skip-duplicate (NuGet) /
 *     --check-url (PyPI) as a second line of defence.
 *   • DRY_RUN (env DRY_RUN=true or `--dry-run`): does the existence checks and
 *     packs/validates, but PUSHES NOTHING and needs NO tokens. This is what you
 *     run to prove the logic locally.
 *   • Isolation: a failure in one language is recorded and the others still run,
 *     but ANY failure makes the whole run exit non-zero — a partial release
 *     surfaces loudly and the idempotent re-run picks up where it left off.
 *
 * sync-versions runs FIRST (real runs only) so every manifest carries the
 * canonical version before we pack. In the normal flow the merged version PR
 * already synced them; this is belt-and-suspenders.
 */
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import https from "node:https";
import process from "node:process";

const root = process.cwd();
const DRY_RUN = process.env.DRY_RUN === "true" || process.argv.includes("--dry-run");

const version = JSON.parse(readFileSync(resolve(root, "typescript/core/package.json"), "utf8")).version;
if (!version) {
    console.error("Unable to read version from typescript/core/package.json");
    process.exit(1);
}

// ── helpers ──────────────────────────────────────────────────────────────────

function run(cmd, args, opts = {}) {
    console.log(`  > ${cmd} ${args.join(" ")}`);
    execFileSync(cmd, args, { stdio: "inherit", cwd: root, ...opts });
}

function hasTool(name) {
    try {
        execFileSync("sh", ["-c", `command -v ${name}`], { stdio: "ignore" });
        return true;
    } catch {
        return false;
    }
}

function httpGet(url) {
    return new Promise((res, rej) => {
        https
            .get(url, { headers: { "user-agent": "smooth-operator-core-ci-publish" } }, (r) => {
                let body = "";
                r.setEncoding("utf8");
                r.on("data", (c) => (body += c));
                r.on("end", () => res({ statusCode: r.statusCode, body }));
            })
            .on("error", rej);
    });
}

// crates.io sparse-index path layout (mirrors smooth/scripts/ci-publish.mjs).
function sparsePath(crate) {
    if (crate.length === 1) return `1/${crate}`;
    if (crate.length === 2) return `2/${crate}`;
    if (crate.length === 3) return `3/${crate[0]}/${crate}`;
    return `${crate.slice(0, 2)}/${crate.slice(2, 4)}/${crate}`;
}

// ── per-registry existence checks (all pure HTTP GET — no auth) ───────────────

async function npmHasVersion(name, ver) {
    const { statusCode, body } = await httpGet(`https://registry.npmjs.org/${name.replace("/", "%2F")}`);
    if (statusCode === 404) return false;
    if (statusCode !== 200) throw new Error(`npm registry returned ${statusCode} for ${name}`);
    return Boolean(JSON.parse(body).versions?.[ver]);
}

async function cratesHasVersion(crate, ver) {
    const { statusCode, body } = await httpGet(`https://index.crates.io/${sparsePath(crate)}`);
    if (statusCode === 404) return false;
    if (statusCode !== 200) throw new Error(`crates.io index returned ${statusCode} for ${crate}`);
    return body
        .split("\n")
        .map((l) => l.trim())
        .filter(Boolean)
        .some((line) => {
            try {
                return JSON.parse(line).vers === ver;
            } catch {
                return false;
            }
        });
}

async function nugetHasVersion(id, ver) {
    const { statusCode, body } = await httpGet(`https://api.nuget.org/v3-flatcontainer/${id.toLowerCase()}/index.json`);
    if (statusCode === 404) return false;
    if (statusCode !== 200) throw new Error(`NuGet flat-container returned ${statusCode} for ${id}`);
    return (JSON.parse(body).versions ?? []).includes(ver);
}

async function pypiHasVersion(name, ver) {
    const { statusCode, body } = await httpGet(`https://pypi.org/pypi/${name}/json`);
    if (statusCode === 404) return false;
    if (statusCode !== 200) throw new Error(`PyPI returned ${statusCode} for ${name}`);
    return Object.prototype.hasOwnProperty.call(JSON.parse(body).releases ?? {}, ver);
}

function gitTagExists(tag) {
    // Local tag OR remote tag counts as "already published".
    const local = execFileSync("git", ["tag", "-l", tag], { cwd: root }).toString().trim();
    if (local) return true;
    const remote = execFileSync("git", ["ls-remote", "--tags", "origin", tag], { cwd: root }).toString().trim();
    return Boolean(remote);
}

function requireEnv(name) {
    if (!process.env[name]) throw new Error(`${name} is not set (required to publish for real)`);
    return process.env[name];
}

// ── per-registry publishers ───────────────────────────────────────────────────
// Each returns a status string. Existence is checked by the caller.

const registries = [
    {
        name: "npm",
        artifact: `@smooai/smooth-operator-core@${version}`,
        exists: () => npmHasVersion("@smooai/smooth-operator-core", version),
        tool: "pnpm",
        publish(dry) {
            run("pnpm", ["--filter", "@smooai/smooth-operator-core", "build"]);
            const flags = ["--filter", "@smooai/smooth-operator-core", "publish", "--no-git-checks", "--access", "public"];
            if (dry) {
                run("pnpm", [...flags, "--dry-run"]);
                return;
            }
            requireEnv("NODE_AUTH_TOKEN");
            run("pnpm", flags, { env: { ...process.env, NPM_CONFIG_PROVENANCE: "true" } });
        },
    },
    {
        name: "crates.io",
        artifact: `smooai-smooth-operator-core@${version}`,
        exists: () => cratesHasVersion("smooai-smooth-operator-core", version),
        tool: "cargo",
        publish(dry) {
            if (dry) {
                // Package the tarball without a full compile — proves the crate
                // is packable without the multi-minute verify build.
                run("cargo", ["package", "-p", "smooai-smooth-operator-core", "--no-verify", "--allow-dirty"], { cwd: resolve(root, "rust") });
                return;
            }
            requireEnv("CARGO_REGISTRY_TOKEN");
            run("cargo", ["publish", "-p", "smooai-smooth-operator-core", "--locked"], { cwd: resolve(root, "rust") });
        },
    },
    {
        name: "NuGet",
        artifact: `SmooAI.SmoothOperator.Core@${version}`,
        exists: () => nugetHasVersion("SmooAI.SmoothOperator.Core", version),
        tool: "dotnet",
        publish(dry) {
            run("dotnet", ["pack", "dotnet/core/src/SmooAI.SmoothOperator.Core.csproj", "-c", "Release", "-o", "dist"]);
            if (!existsSync(resolve(root, "dist"))) throw new Error("dotnet pack produced no dist/ output");
            if (dry) return;
            const apiKey = requireEnv("NUGET_API_KEY");
            run("dotnet", ["nuget", "push", "dist/*.nupkg", "--api-key", apiKey, "--source", "https://api.nuget.org/v3/index.json", "--skip-duplicate"]);
        },
    },
    {
        name: "PyPI",
        artifact: `smooai-smooth-operator-core@${version}`,
        exists: () => pypiHasVersion("smooai-smooth-operator-core", version),
        tool: "uv",
        publish(dry) {
            const cwd = resolve(root, "python/core");
            run("uv", ["build"], { cwd });
            if (dry) return;
            requireEnv("UV_PUBLISH_TOKEN");
            // --check-url makes a re-run of an already-published version a no-op.
            run("uv", ["publish", "--check-url", "https://pypi.org/simple/"], { cwd });
        },
    },
    {
        name: "Go (git tag)",
        artifact: `go/v${version}`,
        exists: async () => gitTagExists(`go/v${version}`),
        tool: "git",
        publish(dry) {
            const tag = `go/v${version}`;
            if (dry) {
                console.log(`  (dry-run) would create + push tag ${tag} at HEAD`);
                return;
            }
            run("git", ["tag", tag]);
            run("git", ["push", "origin", tag]);
        },
    },
];

// ── orchestrate ───────────────────────────────────────────────────────────────

(async () => {
    console.log(`smooth-operator-core release @ ${version}${DRY_RUN ? "  (DRY RUN — nothing is pushed)" : ""}\n`);

    if (!DRY_RUN) {
        console.log("Syncing manifests to canonical version first…");
        run("node", ["scripts/sync-versions.mjs"]);
        console.log("");
    }

    const results = [];

    for (const reg of registries) {
        console.log(`── ${reg.name}: ${reg.artifact}`);
        try {
            if (await reg.exists()) {
                console.log(`  [skip] already published`);
                results.push({ name: reg.name, status: "skipped" });
                continue;
            }

            if (DRY_RUN && !hasTool(reg.tool)) {
                console.log(`  [dry-run] ${reg.tool} not installed — skipping pack (would publish ${reg.artifact})`);
                results.push({ name: reg.name, status: "would-publish (pack skipped: no toolchain)" });
                continue;
            }

            console.log(`  ${DRY_RUN ? "[dry-run] pack/validate — would publish" : "[publish]"} ${reg.artifact}`);
            reg.publish(DRY_RUN);
            results.push({ name: reg.name, status: DRY_RUN ? "would-publish" : "published" });
        } catch (err) {
            // Isolate: record and keep going so one broken language doesn't mask
            // the state of the others. The non-zero exit below still surfaces it.
            console.error(`  [FAIL] ${reg.name}: ${err.message}`);
            results.push({ name: reg.name, status: "FAILED", error: err.message });
        }
    }

    console.log(`\n── Summary (@ ${version}${DRY_RUN ? ", dry run" : ""}) ──`);
    for (const r of results) console.log(`  ${r.name.padEnd(14)} ${r.status}`);

    const failures = results.filter((r) => r.status === "FAILED");
    if (failures.length) {
        console.error(`\n${failures.length} registr${failures.length === 1 ? "y" : "ies"} failed — see above.`);
        process.exit(1);
    }
    console.log(`\nDone.`);
})().catch((err) => {
    console.error(err);
    process.exit(1);
});
