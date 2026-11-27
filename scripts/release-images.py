#!/usr/bin/env python3
"""Digest release protocol. JSON is data, never shell-sourced.

The source catalogue lives in image-service-inputs.sh. This helper handles
baseline snapshots, build receipts and Compose overrides, including legacy
images whose tags may no longer identify the running artifact.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys

SHA = re.compile(r"[0-9a-f]{40}")
REF = re.compile(r"[a-zA-Z0-9._:/-]+@sha256:[0-9a-f]{64}")
SERVICE = re.compile(r"[a-z0-9][a-z0-9-]*")


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def read(path):
    return json.loads(Path(path).read_text())


def write(path, data):
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name(target.name + ".tmp")
    temporary.write_text(json.dumps(data, sort_keys=True, indent=2) + "\n")
    temporary.chmod(0o600)
    temporary.replace(target)


def validate(data):
    if data.get("version") != 1 or not SHA.fullmatch(data.get("release", "")):
        raise ValueError("invalid release manifest version or SHA")
    if not isinstance(data.get("services"), dict):
        raise ValueError("missing service manifest")
    for name, entry in data["services"].items():
        if not SERVICE.fullmatch(name) or not REF.fullmatch(entry.get("image", "")):
            raise ValueError(f"invalid digest reference for {name}")
        source = entry.get("source")
        if source is not None and not SHA.fullmatch(source):
            raise ValueError(f"invalid source SHA for {name}")
        if not isinstance(entry.get("configured"), str):
            raise ValueError(f"missing configured image for {name}")
    return data


def identity(data):
    return hashlib.sha256(json.dumps(data, sort_keys=True).encode()).hexdigest()


def catalogue():
    if os.environ.get("IMAGE_CATALOGUE"):
        return json.loads(os.environ["IMAGE_CATALOGUE"])
    script = Path(__file__).with_name("image-service-inputs.sh")
    rows = run("bash", "-c", 'source "$1"; for s in $(image_input_services); do '
               'echo "$s $(image_service_variable "$s")"; done', "bash", str(script))
    return dict(row.split() for row in rows.splitlines())


def repository(ref):
    ref = ref.split("@")[0]
    ref = ref.rsplit(":", 1)[0] if ":" in ref.rsplit("/", 1)[-1] else ref
    first = ref.split("/", 1)[0]
    if "/" not in ref:
        return "docker.io/library/" + ref
    if "." not in first and ":" not in first and first != "localhost":
        return "docker.io/" + ref
    return ref


def installed(configured, service=None):
    """Return a digest only when it names the installed/running image."""
    image = configured
    if service:
        ids = run("docker", "compose", "ps", "-a", "-q", service).splitlines()
        if ids:
            images = {run("docker", "inspect", "--format", "{{.Image}}", cid) for cid in ids}
            if len(images) != 1:
                raise ValueError(f"multiple installed image identities for {service}")
            image = images.pop()
    metadata = json.loads(run("docker", "image", "inspect", image))[0]
    for digest in metadata.get("RepoDigests") or []:
        if repository(digest) == repository(configured) and REF.fullmatch(digest):
            if json.loads(run("docker", "image", "inspect", digest))[0]["Id"] == metadata["Id"]:
                return digest
    raise ValueError(f"no verifiable installed digest for {service or configured}")


def snapshot(repo):
    root = Path(repo)
    marker = root / ".deployed_sha"
    if not marker.exists():
        if (root / ".deploy/releases").exists() and any((root / ".deploy/releases").iterdir()):
            raise ValueError("release records exist without .deployed_sha")
        return {"version": 1, "release": None, "services": {}}
    sha = marker.read_text().strip()
    if not SHA.fullmatch(sha):
        raise ValueError("invalid deployed SHA")
    release = root / ".deploy/releases" / sha
    if (release / "images.json").exists():
        data = validate(read(release / "images.json"))
        if data["release"] != sha:
            raise ValueError("manifest does not match deployed SHA")
        return data
    # Legacy: preserve source identities, and never resolve a mutable remote tag.
    tags = {}
    if (release / "images.env").exists():
        for line in (release / "images.env").read_text().splitlines():
            key, value = line.split("=", 1)
            if key in tags or not re.fullmatch(r"[A-Z0-9_]+_IMAGE_TAG", key) or not SHA.fullmatch(value):
                raise ValueError("invalid legacy image manifest")
            tags[key] = value
    env = os.environ.copy()
    env.update(tags)
    env["IMAGE_TAG"] = sha
    config = json.loads(run("docker", "compose", "config", "--format", "json", env=env))
    known = catalogue()
    if (release / "images.env").exists():
        missing = set(known.values()) - tags.keys() - {"BITCOIND_RPC_PROXY_IMAGE_TAG"}
        if missing:
            raise ValueError(f"legacy image manifest missing entries: {sorted(missing)}")
    entries = {}
    for name, cfg in config["services"].items():
        ref = cfg.get("image")
        if not ref:
            raise ValueError(f"service {name} has no image")
        entry = {"configured": ref, "source": tags.get(known.get(name), sha) if name in known else None}
        try:
            entry["image"] = installed(ref, name)
        except (subprocess.CalledProcessError, ValueError):
            # CI rebuilds custom entries without digests. Activation separately
            # saves local image IDs for rollback before any container changes.
            entry["image"] = None
        entries[name] = entry
    return {"version": 1, "release": sha, "services": entries, "legacy": True}


def baseline(args):
    os.chdir(args.repo)
    lock = Path(".deploy/deploy.lock")
    if lock.exists():
        with lock.open("r") as handle:
            fcntl.flock(handle, fcntl.LOCK_SH | fcntl.LOCK_NB)
            data = snapshot(args.repo)
    else:
        if Path(".deployed_sha").exists():
            raise ValueError("deployed installation has no deployment lock")
        data = snapshot(args.repo)
    print(json.dumps(data, sort_keys=True))


def receipt(args):
    if not SERVICE.fullmatch(args.service) or not SHA.fullmatch(args.sha) or not REF.fullmatch(args.image):
        raise ValueError("invalid build receipt")
    write(args.output, {"service": args.service, "source": args.sha, "image": args.image})


def prepare(args):
    base = read(args.baseline)
    known = catalogue()
    receipts = {}
    for path in Path(args.receipts).rglob("*.json"):
        item = read(path)
        name = item["service"]
        if name not in known or name in receipts or item["source"] != args.sha or not REF.fullmatch(item["image"]):
            raise ValueError(f"invalid or duplicate build receipt: {path}")
        receipts[name] = item
    changed = set(filter(None, args.changed.split(",")))
    if set(receipts) != changed:
        raise ValueError("build receipts do not exactly match changed services")
    # Rendering uses new source/config, but never floating values for unchanged
    # custom services. Caller has loaded candidate images.env.
    config = json.loads(run("docker", "compose", "config", "--format", "json"))
    # Exact image references shared with a catalogue service define aliases:
    # an unmanaged service running a catalogue service's image (e.g.
    # dexmonitor-public reusing the dexmonitor image in public mode) must be
    # pinned to whatever that catalogue service resolved to, so it moves with
    # every rebuild instead of freezing at its first pulled digest.
    alias_refs = {}
    for name, cfg in config["services"].items():
        if name in known:
            ref = cfg.get("image", "")
            if ref:
                alias_refs.setdefault(ref, name)
    entries = {}
    for name, cfg in config["services"].items():
        configured = cfg.get("image", "")
        prior = base["services"].get(name, {})
        if name in known:
            item = receipts.get(name) or prior
            image = item.get("image")
            if not image or not REF.fullmatch(image):
                raise ValueError(f"{name} requires a build before digest migration")
            # Registry/owner changes must not silently inherit another repository.
            if repository(image) != repository(configured):
                raise ValueError(f"image repository changed for {name}; rebuild required")
            entries[name] = {"configured": configured, "source": item["source"], "image": image}
        else:
            if "build" in cfg:
                raise ValueError(f"unmanaged build service: {name}")
            alias = alias_refs.get(configured)
            if alias is not None:
                item = receipts.get(alias) or base["services"].get(alias, {})
                image = item.get("image")
                if not image or not REF.fullmatch(image):
                    raise ValueError(f"{name} requires a build before digest migration")
            elif prior.get("configured") == configured and prior.get("image"):
                image = prior["image"]
            elif REF.fullmatch(configured):
                image = configured
            else:
                run("docker", "pull", configured)
                image = installed(configured)
            entries[name] = {"configured": configured, "source": None, "image": image}
    # Preserve disabled custom images for future activation and source checks.
    for name in known.keys() - entries.keys():
        item = receipts.get(name) or base["services"].get(name)
        if item and item.get("image"):
            entries[name] = {"configured": item.get("configured", item["image"]),
                             "source": item["source"], "image": item["image"]}
    write(args.output, validate({"version": 1, "release": args.sha, "services": entries}))


def check(args):
    # Caller owns the exclusive deployment lock; do not acquire it recursively.
    if identity(read(args.baseline)) != identity(snapshot(args.repo)):
        raise ValueError("production baseline changed; rerun CI before activation")


def ci_baseline(args):
    env = os.environ
    source = Path(__file__).read_text()
    command = shlex.join(["env", "IMAGE_CATALOGUE=" + json.dumps(catalogue()),
                          "python3", "-c", source, "baseline", "--repo", env["REPO_DIR"]])
    output = run("ssh", "-i", env.get("SSH_IDENTITY", str(Path.home() / ".ssh/id_ed25519")),
                 "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
                 "-o", "ConnectTimeout=10", env["SSH_USER"] + "@" + env["SSH_HOST"], command)
    data = json.loads(output)
    if data.get("version") != 1 or not isinstance(data.get("services"), dict):
        raise ValueError("invalid production snapshot")
    write(args.output, data)


def override(args):
    data = validate(read(args.manifest))
    active = set(run("docker", "compose", "config", "--services").splitlines())
    missing = active - data["services"].keys()
    if missing:
        raise ValueError(f"missing digest entries: {sorted(missing)}")
    write(args.output, {"services": {name: {"image": data["services"][name]["image"]} for name in active}})


def legacy_rollback(args):
    # A local tag prevents garbage collection and restores even images never
    # published to a registry. No mutable registry lookup is involved.
    services = {}
    env = os.environ.copy()
    env["IMAGE_TAG"] = args.sha
    tags = Path(".deploy/releases") / args.sha / "images.env"
    if tags.exists():
        for line in tags.read_text().splitlines():
            key, value = line.split("=", 1)
            if not re.fullmatch(r"[A-Z0-9_]+_IMAGE_TAG", key) or not SHA.fullmatch(value):
                raise ValueError("invalid legacy rollback image manifest")
            env[key] = value
    config = json.loads(run("docker", "compose", "config", "--format", "json", env=env))
    for name, cfg in config["services"].items():
        ids = run("docker", "compose", "ps", "-a", "-q", name).splitlines()
        # `compose config` includes services behind disabled profiles. There is
        # no running artifact to preserve for a profile that was never started.
        if cfg.get("profiles") and not ids:
            continue
        if ids:
            identities = {run("docker", "inspect", "--format", "{{.Image}}", cid) for cid in ids}
            if len(identities) != 1:
                raise ValueError(f"multiple installed image identities for {name}")
            image = identities.pop()
        else:
            image = json.loads(run("docker", "image", "inspect", cfg["image"]))[0]["Id"]
        tag = f"dexarb-rollback/{name}:{args.sha}"
        run("docker", "image", "tag", image, tag)
        services[name] = {"image": tag, "pull_policy": "never"}
    write(args.output, {"services": services})


def publish_stage(args):
    """Publish a complete upload without replacing a live activation's files."""
    root = Path(args.repo).resolve()
    state = root / '.deploy'
    incoming = Path(args.incoming)
    if (not SHA.fullmatch(args.sha) or incoming.is_symlink()
            or incoming.resolve().parent != state / 'incoming'
            or not re.fullmatch(re.escape(args.sha) + r'\.[A-Za-z0-9]{8}', incoming.name)):
        raise ValueError('invalid incoming release directory')
    for name in ['release-images.py', 'image-service-inputs.sh', 'transactional-deploy.sh',
                 'validate-env-contract.sh', 'baseline.json', 'env', 'receipts']:
        if not (incoming / name).exists():
            raise ValueError(f'incomplete staged release: {name}')
    # Dispatch and publication share a short lock. The deploy lock additionally
    # excludes a transaction started outside ci-activate.
    with (state / 'stage.lock').open('a') as dispatch, (state / 'deploy.lock').open('a') as deployment:
        fcntl.flock(dispatch, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(deployment, fcntl.LOCK_EX | fcntl.LOCK_NB)
        run_dir = state / 'runs' / args.sha
        if (run_dir / 'pid').exists() and not (run_dir / 'result').exists():
            pid = (run_dir / 'pid').read_text().strip()
            if not pid.isdecimal() or int(pid) <= 0:
                raise ValueError('invalid activation PID; refusing to replace its stage')
            try:
                os.kill(int(pid), 0)
            except ProcessLookupError:
                pass
            else:
                raise ValueError('activation is running; refusing to replace its stage')
        stage = state / 'staging' / args.sha
        stage.parent.mkdir(parents=True, exist_ok=True)
        if stage.is_symlink():
            raise ValueError('stage must not be a symlink')
        if stage.exists():
            shutil.rmtree(stage)
        incoming.rename(stage)


def main():
    parser = argparse.ArgumentParser(__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    commands = {
        "baseline": (baseline, ["repo"]),
        "ci-baseline": (ci_baseline, ["output"]),
        "check": (check, ["repo", "baseline"]),
        "receipt": (receipt, ["service", "sha", "image", "output"]),
        "prepare": (prepare, ["repo", "baseline", "receipts", "sha", "changed", "output"]),
        "override": (override, ["manifest", "output"]),
        "legacy-rollback": (legacy_rollback, ["sha", "output"]),
        "publish-stage": (publish_stage, ["repo", "incoming", "sha"]),
    }
    for name, (function, arguments) in commands.items():
        command = sub.add_parser(name)
        for argument in arguments:
            command.add_argument("--" + argument, required=True)
        command.set_defaults(function=function)
    args = parser.parse_args()
    try:
        args.function(args)
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"release images: {error}\n")


if __name__ == "__main__":
    main()
