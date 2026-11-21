#!/usr/bin/env python3
"""Static checks for the provisioned Grafana dashboards.

Catches the failure modes that have silently broken dashboards in production:

* uid changes on an already-provisioned file. Grafana keeps the provisioning
  record keyed by file path; when the file's uid changes it tries to update the
  old dashboard under the new uid and fails every 30s with
  "could not resolve dashboards:uid:<new>: Dashboard not found" — the stale
  copy keeps being served. Moving or renaming the file at the same time is
  safe. Pass --base <git-ref> to compare against it.
* missing / duplicate uids and titles (Grafana requires both unique).
* datasource uids that are not provisioned.
* LogQL double-quoted strings with escapes Go/Loki reject (e.g. "\\[" written
  as "\\[" inside "..."): Loki answers "invalid char escape" and the panel errors.
  Use backtick strings for regexes.

Usage: scripts/validate-grafana-dashboards.py [--base REF]
"""
import argparse
import json
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DASH_DIR = ROOT / "deploy/monitoring/grafana/dashboards"
DS_DIR = ROOT / "deploy/monitoring/grafana/provisioning/datasources"
BUILTIN_DS = {"-- Mixed --", "-- Grafana --", "-- Dashboard --", "__expr__", "grafana"}
# Escapes accepted inside a Go double-quoted string literal.
VALID_ESCAPE = set('abfnrtv\\"\'xuU01234567')
DQ_STRING = re.compile(r'"((?:[^"\\]|\\.)*)"')


def walk(panels):
    for p in panels:
        yield p
        yield from walk(p.get("panels", []))


def provisioned_uids():
    uids = set()
    for f in DS_DIR.glob("*.y*ml"):
        uids.update(re.findall(r"^\s*uid:\s*([\w-]+)\s*$", f.read_text(), re.M))
    return uids


NEW_FILE = object()


def base_uid(ref, rel):
    """uid of rel at ref; None if the file existed without a uid (Grafana then
    assigned a random one), NEW_FILE if it did not exist."""
    try:
        raw = subprocess.run(["git", "show", f"{ref}:{rel}"], cwd=ROOT, capture_output=True, text=True, check=True).stdout
    except subprocess.CalledProcessError:
        return NEW_FILE
    try:
        return json.loads(raw).get("uid")
    except json.JSONDecodeError:
        return NEW_FILE


def bad_logql_escapes(expr):
    out = []
    for m in DQ_STRING.finditer(expr):
        s = m.group(1)
        i = 0
        while i < len(s):
            if s[i] == "\\":
                if i + 1 < len(s) and s[i + 1] not in VALID_ESCAPE:
                    out.append(s[i:i + 2])
                i += 2
            else:
                i += 1
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", help="git ref to compare dashboard uids against (e.g. origin/main)")
    args = ap.parse_args()

    ds_uids = provisioned_uids() | BUILTIN_DS
    errors, uids, titles = [], {}, {}

    for f in sorted(DASH_DIR.rglob("*.json")):
        rel = f.relative_to(ROOT).as_posix()
        try:
            d = json.loads(f.read_text())
        except json.JSONDecodeError as e:
            errors.append(f"{rel}: invalid JSON: {e}")
            continue

        uid, title = d.get("uid"), d.get("title")
        if not uid:
            errors.append(f"{rel}: missing uid (Grafana would assign a random one; cross-links and agents need it stable)")
        elif uid in uids:
            errors.append(f"{rel}: uid {uid!r} also used by {uids[uid]}")
        else:
            uids[uid] = rel
        if title in titles:
            errors.append(f"{rel}: title {title!r} also used by {titles[title]}")
        titles[title] = rel

        if args.base and uid:
            old = base_uid(args.base, rel)
            if old is not NEW_FILE and old != uid:
                was = repr(old) if old else "a Grafana-assigned random uid"
                errors.append(
                    f"{rel}: uid changed from {was} to {uid!r}. Grafana cannot re-key a provisioned dashboard: it logs "
                    f"'could not resolve dashboards:uid:{uid}' every 30s and keeps serving the old copy (and the API "
                    f"refuses to delete provisioned dashboards). Keep the old uid, or move/rename the file in the same "
                    f"change: Grafana then drops the old copy and creates the new uid cleanly.")

        ids = [p.get("id") for p in walk(d.get("panels", []))]
        dup = {i for i in ids if ids.count(i) > 1}
        if dup:
            errors.append(f"{rel}: duplicate panel ids {sorted(dup)}")

        for p in walk(d.get("panels", [])):
            refs = [p.get("datasource")] + [t.get("datasource") for t in p.get("targets", [])]
            for ds in refs:
                ds_uid = ds.get("uid") if isinstance(ds, dict) else ds
                if ds_uid and not str(ds_uid).startswith("$") and ds_uid not in ds_uids:
                    errors.append(f"{rel}: panel {p.get('title')!r} uses unknown datasource uid {ds_uid!r}")
            for t in p.get("targets", []):
                ds = t.get("datasource") or p.get("datasource") or {}
                if isinstance(ds, dict) and ds.get("uid") == "loki" and t.get("expr"):
                    bad = bad_logql_escapes(t["expr"])
                    if bad:
                        errors.append(f"{rel}: panel {p.get('title')!r} LogQL has invalid escape(s) {bad} in a \"...\" string; use backticks")

    for e in errors:
        print(f"ERROR {e}")
    print(f"{len(uids)} dashboards checked, {len(errors)} problem(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
