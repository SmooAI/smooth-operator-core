"""Consumer deny-policy tests — ported from the Rust ``deny_policy.rs`` test module."""

from __future__ import annotations

from smooth_operator_core.deny_policy import (
    DenyPolicy,
    DenyPredicate,
    DenyReason,
    DenyRules,
    glob_match,
)
from smooth_operator_core.hooks import ToolCall


def call(name: str, args: dict) -> ToolCall:
    return ToolCall(name=name, arguments=args)


def bash_call(cmd: str) -> ToolCall:
    return call("bash", {"cmd": cmd})


# ── glob matcher ───────────────────────────────────────────────
def test_glob_exact_and_wildcards():
    assert glob_match("exact", "exact")
    assert not glob_match("exact", "exacts")
    assert glob_match("vendor.*", "vendor.delete")
    assert not glob_match("vendor.*", "other.delete")
    assert glob_match("*.delete", "vendor.delete")
    assert not glob_match("*.delete", "vendor.deleted")
    assert glob_match("a*c", "abc")
    assert glob_match("a*c", "ac")
    assert not glob_match("a*c", "ab")
    assert glob_match("/prod/**", "/prod/secrets/db.txt")
    assert not glob_match("/prod/**", "/staging/x")
    assert glob_match("**/secrets/**", "/a/b/secrets/c/d")
    assert not glob_match("**/secrets/**", "/a/b/c")


# ── declarative: tools ─────────────────────────────────────────
def test_tools_section_denies_match_allows_nonmatch():
    policy = DenyPolicy.from_toml(
        """
        [tools]
        deny = ["vendor.dangerous_tool", "*.delete_prod"]
        """
    )
    assert policy.evaluate(call("vendor.dangerous_tool", {})) is not None
    assert policy.evaluate(call("svc.delete_prod", {})) is not None
    assert policy.evaluate(call("vendor.safe_tool", {})) is None


# ── declarative: bash ──────────────────────────────────────────
def test_bash_section_denies_match_allows_nonmatch():
    policy = DenyPolicy.from_toml(
        """
        [bash]
        deny_patterns = ["aws * --profile prod", "terraform apply"]
        """
    )
    assert policy.evaluate(bash_call("aws s3 ls --profile prod")) is not None
    assert policy.evaluate(bash_call("terraform apply -auto-approve")) is not None
    assert policy.evaluate(bash_call("aws s3 ls --profile dev")) is None
    assert policy.evaluate(bash_call("aws s3 ls")) is None


def test_bash_prefix_word_boundary():
    policy = DenyPolicy.from_toml(
        """
        [bash]
        deny_patterns = ["aws "]
        """
    )
    assert policy.evaluate(bash_call("aws s3 ls")) is not None
    assert policy.evaluate(bash_call("awslocal s3 ls")) is None


def test_bash_deny_survives_sudo_and_compound_and_extra_flags():
    policy = DenyPolicy.from_toml(
        """
        [bash]
        deny_patterns = ["aws * --profile prod"]
        """
    )
    assert policy.evaluate(bash_call("sudo aws s3 rm s3://b --profile prod")) is not None
    assert policy.evaluate(bash_call("ls && aws s3 ls --profile prod")) is not None
    assert policy.evaluate(bash_call("aws s3 ls --profile prod --region us-east-1")) is not None
    assert policy.evaluate(bash_call("timeout 5 aws s3 ls --profile prod")) is not None


# ── declarative: network ───────────────────────────────────────
def test_network_section_denies_suffix_and_glob():
    policy = DenyPolicy.from_toml(
        """
        [network]
        deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com", "secrets.example.com"]
        """
    )
    assert policy.evaluate(call("web_fetch", {"url": "https://api.prod.internal/x"})) is not None
    assert policy.evaluate(call("web_fetch", {"url": "https://prod.internal/"})) is not None
    assert policy.evaluate(call("web_fetch", {"url": "https://prod-db1.rds.amazonaws.com"})) is not None
    assert policy.evaluate(call("web_fetch", {"host": "api.secrets.example.com"})) is not None
    assert policy.evaluate(call("web_fetch", {"url": "https://staging.internal/x"})) is None
    assert policy.evaluate(bash_call("curl https://api.prod.internal/health")) is not None


# ── declarative: paths ─────────────────────────────────────────
def test_paths_section_denies_write_and_read():
    policy = DenyPolicy.from_toml(
        """
        [paths]
        deny = ["/prod/**", "**/secrets/**"]
        """
    )
    assert policy.evaluate(call("file_write", {"path": "/prod/config.yaml"})) is not None
    assert policy.evaluate(call("read_file", {"path": "/app/secrets/db.env"})) is not None
    assert policy.evaluate(call("list_dir", {"dir": "/prod/data"})) is not None
    assert policy.evaluate(call("file_write", {"path": "/app/src/main.rs"})) is None


# ── predicate tier ─────────────────────────────────────────────
class _ProdAccountPredicate(DenyPredicate):
    def evaluate(self, c: ToolCall) -> DenyReason | None:
        cmd = c.arguments.get("cmd", "") if isinstance(c.arguments, dict) else ""
        return DenyReason.new("resolved to the prod AWS account") if "999999999999" in cmd else None


def test_predicate_some_denies_none_falls_through():
    policy = DenyPolicy().with_predicate(_ProdAccountPredicate())
    denied = policy.evaluate(bash_call("aws s3 ls --profile acct-999999999999"))
    assert denied is not None and "prod AWS account" in denied
    assert policy.evaluate(bash_call("aws s3 ls --profile acct-111")) is None


# ── empty policy = no-op ───────────────────────────────────────
def test_empty_policy_denies_nothing():
    policy = DenyPolicy()
    assert policy.is_empty()
    assert policy.evaluate(bash_call("rm -rf /prod")) is None
    assert policy.evaluate(call("file_write", {"path": "/prod/x"})) is None
    assert policy.evaluate(call("vendor.anything", {})) is None


# ── TOML round-trip ────────────────────────────────────────────
def test_toml_round_trip():
    rules = DenyRules.new()
    rules.tools_deny.add("vendor.dangerous_tool")
    rules.bash_deny_patterns.add("aws * --profile prod")
    rules.network_deny_hosts.add("*.prod.internal")
    rules.paths_deny.add("/prod/**")
    text = rules.to_toml_string()
    parsed = DenyRules.parse(text)
    assert parsed.tools_deny == rules.tools_deny
    assert parsed.bash_deny_patterns == rules.bash_deny_patterns
    assert parsed.network_deny_hosts == rules.network_deny_hosts
    assert parsed.paths_deny == rules.paths_deny


def test_empty_rules_parse_and_are_empty():
    assert DenyRules.parse("").is_empty()
    assert DenyRules.parse("schema_version = 1").is_empty()


# ── precedence: declarative before predicate ───────────────────
class _AlwaysDeny(DenyPredicate):
    def evaluate(self, c: ToolCall) -> DenyReason | None:
        return DenyReason.new("predicate always denies")


def test_declarative_reason_wins_over_predicate():
    policy = DenyPolicy.from_toml(
        """
        [tools]
        deny = ["vendor.tool"]
        """
    ).with_predicate(_AlwaysDeny())
    r1 = policy.evaluate(call("vendor.tool", {}))
    assert r1 is not None and "(tools)" in r1
    r2 = policy.evaluate(call("other.tool", {}))
    assert r2 is not None and "(predicate)" in r2
