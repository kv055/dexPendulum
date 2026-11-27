#!/usr/bin/env python3
"""Protocol tests with a mocked Docker boundary; no production access."""
import argparse
import copy
import fcntl
import importlib.util
import json
import os
import re
import shutil
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("release_images", ROOT / "scripts/release-images.py")
images = importlib.util.module_from_spec(spec)
spec.loader.exec_module(images)
A, B = "a" * 40, "b" * 40
D1, D2 = "1" * 64, "2" * 64


def entry(service, sha=A, digest=D1):
    repo = "ghcr.io/test/dexarbtrading-" + service
    return {"source": sha, "configured": repo + ":" + sha, "image": repo + "@sha256:" + digest}


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.base = {"version": 1, "release": A, "services": {
            "bisq2": entry("bisq2"), "unified-api": entry("unified-api")}}
        self.baseline = self.root / "baseline.json"
        images.write(self.baseline, self.base)
        self.receipts = self.root / "receipts"
        self.receipts.mkdir()
        self.output = self.root / "candidate.json"
        self.args = argparse.Namespace(repo=str(self.root), baseline=str(self.baseline),
            receipts=str(self.receipts), sha=B, changed="unified-api", output=str(self.output))
        images.receipt(argparse.Namespace(service="unified-api", sha=B,
            image=entry("unified-api", B, D2)["image"], output=str(self.receipts / "api.json")))
        self.config = {"services": {s: {"image": entry(s, B if s == "unified-api" else A)["configured"]}
                                    for s in self.base["services"]}}

    def prepare(self):
        with patch.object(images, "run", return_value=json.dumps(self.config)):
            # catalogue executes bash; keep it independent of Docker mocking.
            with patch.object(images, "catalogue", return_value={"bisq2": "BISQ2_IMAGE_TAG", "unified-api": "UNIFIED_API_IMAGE_TAG"}):
                images.prepare(self.args)
        return images.read(self.output)

    def test_inherits_exact_digest_and_source(self):
        candidate = self.prepare()
        self.assertEqual(candidate["services"]["bisq2"], self.base["services"]["bisq2"])
        self.assertEqual(candidate["services"]["unified-api"]["image"], entry("unified-api", B, D2)["image"])
        self.assertEqual(candidate["services"]["unified-api"]["source"], B)

    def test_missing_receipt_rejected(self):
        (self.receipts / "api.json").unlink()
        with self.assertRaisesRegex(ValueError, "exactly match"):
            self.prepare()

    def test_duplicate_receipt_rejected(self):
        images.write(self.receipts / "duplicate.json", images.read(self.receipts / "api.json"))
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.prepare()

    def test_receipt_wrong_source_rejected(self):
        item = images.read(self.receipts / "api.json")
        item["source"] = A
        images.write(self.receipts / "api.json", item)
        with self.assertRaises(ValueError):
            self.prepare()

    def test_baseline_race_rejected(self):
        actual = copy.deepcopy(self.base)
        actual["services"]["bisq2"]["image"] = entry("bisq2", A, D2)["image"]
        with patch.object(images, "snapshot", return_value=actual):
            with self.assertRaisesRegex(ValueError, "baseline changed"):
                images.check(self.args)

    def test_matching_baseline_accepted(self):
        with patch.object(images, "snapshot", return_value=self.base):
            images.check(self.args)

    def test_config_only_empty_receipts(self):
        (self.receipts / "api.json").unlink()
        self.args.changed = ""
        candidate = self.prepare()
        self.assertEqual(candidate["services"]["unified-api"]["image"], entry("unified-api")["image"])

    def test_upstream_digest_inherited_without_pull(self):
        self.base["services"]["postgres"] = {"configured": "postgres:17-alpine",
            "source": None, "image": "postgres@sha256:" + D1}
        images.write(self.baseline, self.base)
        self.config["services"]["postgres"] = {"image": "postgres:17-alpine"}
        candidate = self.prepare()
        self.assertEqual(candidate["services"]["postgres"], self.base["services"]["postgres"])

    def test_alias_pins_tracked_service_digest(self):
        # An unmanaged service sharing a catalogue service's exact image
        # reference must follow that service's rebuild instead of freezing.
        self.config["services"]["unified-api-public"] = {
            "image": self.config["services"]["unified-api"]["image"]}
        candidate = self.prepare()
        alias = candidate["services"]["unified-api-public"]
        self.assertEqual(alias["image"], candidate["services"]["unified-api"]["image"])
        self.assertIsNone(alias["source"])
        self.assertEqual(alias["configured"], self.config["services"]["unified-api"]["image"])

    def test_alias_requires_tracked_service_build(self):
        # No receipt and no prior digest for the aliased service: the alias
        # cannot inherit a floating tag.
        self.config["services"]["unified-api-public"] = {
            "image": self.config["services"]["unified-api"]["image"]}
        self.base["services"] = {"bisq2": entry("bisq2")}
        images.write(self.baseline, self.base)
        (self.receipts / "api.json").unlink()
        self.args.changed = ""
        with self.assertRaisesRegex(ValueError, "requires a build"):
            self.prepare()

    def test_non_alias_keeps_prior_digest(self):
        # Unmanaged services with their own image keep the existing
        # freeze-at-first-pull behaviour (prior digest when configured
        # is unchanged).
        self.config["services"]["nginx"] = {"image": "nginx:1.31-alpine"}
        self.base["services"]["nginx"] = {"configured": "nginx:1.31-alpine",
            "source": None, "image": "nginx@sha256:" + D1}
        images.write(self.baseline, self.base)
        candidate = self.prepare()
        self.assertEqual(candidate["services"]["nginx"]["image"], "nginx@sha256:" + D1)

    def test_changed_repository_requires_build(self):
        self.config["services"]["bisq2"]["image"] = "ghcr.io/another/bisq2:" + A
        with self.assertRaisesRegex(ValueError, "repository changed"):
            self.prepare()

    def test_legacy_missing_digest_requires_build(self):
        self.base["services"]["bisq2"]["image"] = None
        images.write(self.baseline, self.base)
        with self.assertRaisesRegex(ValueError, "requires a build"):
            self.prepare()

    def test_digest_validation_rejects_floating_tag(self):
        self.base["services"]["bisq2"]["image"] = "bisq2:latest"
        with self.assertRaises(ValueError):
            images.validate(self.base)

    def test_migration_uses_running_image_not_retagged_sha(self):
        configured = entry("bisq2")["configured"]
        digest = entry("bisq2")["image"]
        def docker(*args, **kwargs):
            if args[:3] == ("docker", "compose", "ps"):
                return "container"
            if args[:2] == ("docker", "inspect"):
                return "sha256:running"
            if args[:3] == ("docker", "image", "inspect"):
                self.assertIn(args[3], ["sha256:running", digest])
                return json.dumps([{"Id": "sha256:running", "RepoDigests": [digest]}])
            self.fail(str(args))
        with patch.object(images, "run", side_effect=docker):
            self.assertEqual(images.installed(configured, "bisq2"), digest)

    def test_override_has_only_digest_images(self):
        images.write(self.output, self.base)
        override = self.root / "compose.json"
        with patch.object(images, "run", return_value="bisq2\nunified-api"):
            images.override(argparse.Namespace(manifest=str(self.output), output=str(override)))
        self.assertEqual(images.read(override)["services"]["bisq2"], {"image": entry("bisq2")["image"]})

    @unittest.skipUnless(shutil.which('docker'), 'Docker CLI is unavailable')
    def test_real_compose_merges_digest_override_without_daemon(self):
        compose = self.root / 'compose.json'
        override = self.root / 'override.json'
        config = copy.deepcopy(self.config)
        config['services']['bisq2']['environment'] = {'PRESERVE_ME': 'yes'}
        config['services']['unified-api']['depends_on'] = ['bisq2']
        images.write(compose, config)
        with patch.dict(os.environ, {'COMPOSE_FILE': str(compose), 'COMPOSE_PROJECT_NAME': 'digest-protocol-test'}):
            images.prepare(self.args)
            images.override(argparse.Namespace(manifest=str(self.output), output=str(override)))
            merged = json.loads(images.run('docker', 'compose', '-f', str(compose), '-f', str(override),
                                           'config', '--format', 'json'))
        self.assertEqual(merged['services']['bisq2']['image'], entry('bisq2')['image'])
        self.assertEqual(merged['services']['unified-api']['image'], entry('unified-api', B, D2)['image'])
        self.assertEqual(merged['services']['bisq2']['environment']['PRESERVE_ME'], 'yes')
        self.assertIn('bisq2', merged['services']['unified-api']['depends_on'])

    def test_empty_and_corrupt_installations(self):
        self.assertIsNone(images.snapshot(self.root)["release"])
        (self.root / ".deployed_sha").write_text("not a sha")
        with self.assertRaises(ValueError):
            images.snapshot(self.root)

    def test_legacy_rollback_skips_unstarted_optional_service(self):
        config = {'services': {'api-docs': {'image': 'swaggerapi/swagger-ui:v5', 'profiles': ['docs']}}}
        def docker(*args, **kwargs):
            if args[:3] == ('docker', 'compose', 'config'):
                return json.dumps(config)
            if args[:3] == ('docker', 'compose', 'ps'):
                return ''
            self.fail('must not inspect or pull an unstarted optional image: ' + str(args))
        with patch.object(images, 'run', side_effect=docker):
            images.legacy_rollback(argparse.Namespace(sha=A, output=str(self.output)))
        self.assertEqual(images.read(self.output), {'services': {}})

    def test_legacy_rollback_preserves_started_profile(self):
        config = {'services': {'api-docs': {'image': 'swaggerapi/swagger-ui:v5', 'profiles': ['docs']}}}
        commands = []
        def docker(*args, **kwargs):
            commands.append(args)
            if args[:3] == ('docker', 'compose', 'config'):
                return json.dumps(config)
            if args[:3] == ('docker', 'compose', 'ps'):
                return 'container'
            if args[:2] == ('docker', 'inspect'):
                return 'sha256:' + D1
            if args[:3] == ('docker', 'image', 'tag'):
                return ''
            self.fail(str(args))
        with patch.object(images, 'run', side_effect=docker):
            images.legacy_rollback(argparse.Namespace(sha=A, output=str(self.output)))
        self.assertIn(('docker', 'image', 'tag', 'sha256:' + D1, 'dexarb-rollback/api-docs:' + A), commands)
        self.assertEqual(images.read(self.output)['services']['api-docs']['pull_policy'], 'never')


class StagePublicationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / '.deploy'
        self.incoming = self.state / 'incoming' / (A + '.12345678')
        self.incoming.mkdir(parents=True)
        for name in ['release-images.py', 'image-service-inputs.sh', 'transactional-deploy.sh',
                     'validate-env-contract.sh', 'baseline.json']:
            (self.incoming / name).write_text('new')
        for name in ['env', 'receipts']:
            (self.incoming / name).mkdir()
        self.stage = self.state / 'staging' / A
        self.stage.mkdir(parents=True)
        (self.stage / 'sentinel').write_text('old')
        self.args = argparse.Namespace(repo=str(self.root), incoming=str(self.incoming), sha=A)

    def test_replaces_completed_stage(self):
        images.publish_stage(self.args)
        self.assertFalse(self.incoming.exists())
        self.assertFalse((self.stage / 'sentinel').exists())
        self.assertEqual((self.stage / 'baseline.json').read_text(), 'new')

    def test_incomplete_upload_preserves_previous_stage(self):
        (self.incoming / 'baseline.json').unlink()
        with self.assertRaisesRegex(ValueError, 'incomplete staged release'):
            images.publish_stage(self.args)
        self.assertEqual((self.stage / 'sentinel').read_text(), 'old')

    def test_active_dispatch_preserves_previous_stage(self):
        run_dir = self.state / 'runs' / A
        run_dir.mkdir(parents=True)
        (run_dir / 'pid').write_text(str(os.getpid()))
        with self.assertRaisesRegex(ValueError, 'activation is running'):
            images.publish_stage(self.args)
        self.assertEqual((self.stage / 'sentinel').read_text(), 'old')

    def test_locks_preserve_previous_stage(self):
        for name in ['stage.lock', 'deploy.lock']:
            with self.subTest(lock=name), (self.state / name).open('a') as handle:
                fcntl.flock(handle, fcntl.LOCK_EX)
                with self.assertRaises(BlockingIOError):
                    images.publish_stage(self.args)
            self.assertEqual((self.stage / 'sentinel').read_text(), 'old')


class CatalogueTests(unittest.TestCase):
    def test_gitea_static_matrix_matches_catalogue(self):
        workflow = (ROOT / ".gitea/workflows/ci.yml").read_text()
        block = workflow.split('  build-and-push:', 1)[1].split('    steps:', 1)[0]
        rows = [dict(zip(['service', 'context', 'dockerfile'], row)) for row in re.findall(
            r'- service: (\S+)\s+context: (\S+)\s+dockerfile: (\S+)', block)]
        self.assertEqual({row["service"] for row in rows}, set(images.catalogue()))
        for row in rows:
            command = 'source "$1"; image_service_build "$2"'
            expected = subprocess.check_output(["bash", "-c", command, "bash",
                str(ROOT / "scripts/image-service-inputs.sh"), row["service"]], text=True).split()
            self.assertEqual([row["context"].removeprefix("./"), row["dockerfile"]], expected)


class DetectionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ['detect-changed-services.sh', 'image-service-inputs.sh']:
            target = self.root / 'scripts' / name
            target.parent.mkdir(exist_ok=True)
            target.write_text((ROOT / 'scripts' / name).read_text())
        self.git('init', '-q')
        self.git('config', 'user.name', 'Test')
        self.git('config', 'user.email', 'test@example.invalid')
        self.change('dexUnifiedApi/cmd/server/main.go', 'api old')
        self.change('dexMonitor/internal/server.go', 'monitor old')
        self.base = self.commit()
        self.baseline = self.root / 'baseline.json'
        images.write(self.baseline, {'version': 1, 'release': self.base,
            'services': {s: entry(s, self.base) for s in images.catalogue()}})

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def change(self, path, value):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(value)

    def commit(self):
        self.git('add', 'scripts', 'dexUnifiedApi', 'dexMonitor')
        self.git('commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    def detect(self, before, after):
        output = self.root / 'outputs'
        output.write_text('')
        env = {**os.environ, 'EVENT': 'push', 'REF': 'refs/heads/main', 'BEFORE': before,
               'AFTER': after, 'BASELINE_FILE': str(self.baseline), 'GITHUB_OUTPUT': str(output)}
        result = subprocess.run(['bash', str(self.root / 'scripts/detect-changed-services.sh')],
            env=env, capture_output=True, text=True)
        return result, output.read_text()

    def test_failed_release_remains_in_next_build_range(self):
        self.change('dexMonitor/internal/server.go', 'failed deployment')
        failed = self.commit()
        self.change('dexUnifiedApi/cmd/server/main.go', 'next push')
        target = self.commit()
        result, output = self.detect(failed, target)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('changes=unified-api,dexmonitor\n', output)

    def test_missing_baseline_fails_instead_of_using_before(self):
        self.baseline.unlink()
        result, _ = self.detect(self.base, self.base)
        self.assertNotEqual(result.returncode, 0)

    def test_test_only_change_runs_module_without_build(self):
        self.change('dexUnifiedApi/cmd/server/main_test.go', 'test')
        target = self.commit()
        result, output = self.detect(self.base, target)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('changes=\n', output)
        self.assertIn('test_modules=dexUnifiedApi\n', output)

    def test_uninspectable_source_rebuilds_service(self):
        data = images.read(self.baseline)
        data['services']['bisq2']['source'] = 'e' * 40
        images.write(self.baseline, data)
        result, output = self.detect(self.base, self.base)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('changes=bisq2\n', output)

    def _services_block(self, output):
        marker = 'build_services_json<<EOF\n'
        start = output.index(marker) + len(marker)
        end = output.index('\nEOF', start)
        return json.loads(output[start:end])

    def test_build_services_json_has_names_not_paths(self):
        self.change('dexMonitor/internal/server.go', 'failed deployment')
        failed = self.commit()
        self.change('dexUnifiedApi/cmd/server/main.go', 'next push')
        target = self.commit()
        result, output = self.detect(failed, target)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('build_matrix_json', output)
        services = self._services_block(output)
        self.assertIsInstance(services, list)
        self.assertTrue(services)
        for name in services:
            self.assertNotIn('/', name)
        changes = next(line for line in output.splitlines() if line.startswith('changes='))
        self.assertEqual(set(services), set(changes.removeprefix('changes=').split(',')))

    def test_bitcoind_service_name_is_secret_safe(self):
        # Regression test: the bitcoind-rpc-proxy build context is
        # deploy/bitcoind-rpc-proxy, which contains a common secret substring.
        # The services output must carry the bare name only so GitHub never
        # drops it with "Skip output ... since it may contain secret".
        self.change('deploy/bitcoind-rpc-proxy/Dockerfile', 'proxy old')
        self.git('add', 'scripts', 'dexUnifiedApi', 'dexMonitor', 'deploy/bitcoind-rpc-proxy/Dockerfile')
        self.git('commit', '-qm', 'fixture proxy')
        base = self.git('rev-parse', 'HEAD')
        data = images.read(self.baseline)
        data['release'] = base
        for service in images.catalogue():
            data['services'][service] = entry(service, base)
        images.write(self.baseline, data)
        self.change('deploy/bitcoind-rpc-proxy/Dockerfile', 'proxy new')
        self.git('add', 'deploy/bitcoind-rpc-proxy/Dockerfile')
        self.git('commit', '-qm', 'proxy change')
        target = self.git('rev-parse', 'HEAD')
        result, output = self.detect(base, target)
        self.assertEqual(result.returncode, 0, result.stderr)
        changes = next(line for line in output.splitlines() if line.startswith('changes='))
        self.assertIn('bitcoind-rpc-proxy', changes.removeprefix('changes=').split(','))
        services = self._services_block(output)
        self.assertIn('bitcoind-rpc-proxy', services)
        block = output[output.index('build_services_json<<EOF'):output.index('\nEOF', output.index('build_services_json<<EOF'))]
        self.assertNotIn('deploy/', block)


if __name__ == "__main__":
    unittest.main()
