"""Exercise installer downloads and Docker commands without network or services."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.bin = self.base / 'bin'
        self.bin.mkdir()
        self.target = self.base / 'deployment'
        self.calls = self.base / 'calls'
        self.asset = self.base / 'asset'
        self.asset.write_text('#!/bin/sh\necho easy-proxies\n')
        self.checksum = self.base / 'checksum'
        self.checksum.write_text(hashlib.sha256(self.asset.read_bytes()).hexdigest() + '  binary\n')
        self.env = dict(os.environ, PATH=f'{self.bin}:' + os.environ['PATH'],
                        FIXTURES=str(self.base), PROJECT=str(ROOT))
        self.mock('uname', '[[ "$1" == -s ]] && echo Linux || echo "${TEST_ARCH:-x86_64}"')
        self.mock('curl', '''
printf '%s\n' "$*" >> "$FIXTURES/calls"
output=
url=
while (($#)); do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --write-out) shift 2 ;;
    https://*) url=$1; shift ;;
    --retry) shift 2 ;;
    *) shift ;;
  esac
done
case "$url" in
  */releases/latest) printf 'https://github.com/x0rz4/EasyProxiesV2/releases/tag/v2.1.0' ;;
  */config.example.yaml) cp "$PROJECT/config.example.yaml" "$output" ;;
  *.sha256) cp "$FIXTURES/checksum" "$output" ;;
  */easy-proxies-linux-*) cp "$FIXTURES/asset" "$output" ;;
  *) exit 9 ;;
esac
''')
        self.mock('docker', '''
printf 'docker %s\n' "$*" >> "$FIXTURES/calls"
case "$1" in
  pull) [[ "${FAIL_PULL:-0}" != 1 ]] ;;
  container) [[ "${EXISTING_CONTAINER:-0}" == 1 ]] ;;
  inspect) echo unrelated-owner ;;
  *) exit 0 ;;
esac
''')

    def mock(self, name, body):
        path = self.bin / name
        path.write_text('#!/usr/bin/env bash\nset -eu\n' + body + '\n')
        path.chmod(0o755)

    def run_install(self, *args, ok=True):
        result = subprocess.run(['bash', str(ROOT / 'install.sh'), '--dir', str(self.target), *args],
                                env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        return result

    def test_binary_latest_is_pinned_and_config_preserved_on_update(self):
        self.run_install('--no-start')
        self.assertEqual((self.target / 'installed-version').read_text(), 'v2.1.0\n')
        config = self.target / 'config.yaml'
        self.assertNotIn('password: "123456"', config.read_text())
        self.assertEqual(config.stat().st_mode & 0o777, 0o600)
        self.assertTrue(os.access(self.target / 'easy-proxies', os.X_OK))
        config.write_text('custom config\n')
        (self.target / 'data' / 'data.db').write_text('existing database')
        self.run_install('--version', 'v2.1.0', '--no-start')
        self.assertEqual(config.read_text(), 'custom config\n')
        self.assertEqual((self.target / 'data' / 'data.db').read_text(), 'existing database')
        self.assertIn('/v2.1.0/easy-proxies-linux-amd64', self.calls.read_text())

    def test_arm64_asset(self):
        self.env['TEST_ARCH'] = 'aarch64'
        self.run_install('--version', '2.1.0', '--no-start')
        self.assertIn('/v2.1.0/easy-proxies-linux-arm64', self.calls.read_text())

    def test_bad_checksum_keeps_existing_binary_and_config(self):
        self.target.mkdir()
        (self.target / 'easy-proxies').write_text('old binary')
        (self.target / 'config.yaml').write_text('old config')
        self.checksum.write_text('0' * 64 + '  binary\n')
        self.run_install('--version', '2.1.0', '--no-start', ok=False)
        self.assertEqual((self.target / 'easy-proxies').read_text(), 'old binary')
        self.assertEqual((self.target / 'config.yaml').read_text(), 'old config')

    def test_docker_uses_release_version_and_persistent_mounts(self):
        self.run_install('--method', 'docker', '--version', 'v2.1.0')
        calls = self.calls.read_text()
        self.assertIn('docker pull ghcr.io/x0rz4/easyproxiesv2:2.1.0', calls)
        self.assertIn('docker run -d', calls)
        self.assertIn('--network host', calls)
        self.assertIn('dst=/etc/easy-proxies/data', calls)
        self.assertNotIn(':latest', calls)

    def test_docker_no_start_does_not_replace_container(self):
        self.run_install('--method', 'docker', '--version', '2.1.0', '--no-start')
        calls = self.calls.read_text()
        self.assertNotIn('docker rm', calls)
        self.assertNotIn('docker run', calls)

    def test_failed_pull_does_not_remove_container(self):
        self.env['FAIL_PULL'] = '1'
        self.run_install('--method', 'docker', '--version', '2.1.0', ok=False)
        self.assertNotIn('docker rm', self.calls.read_text())
        self.assertFalse((self.target / 'installed-version').exists())

    def test_prerelease_is_pinned(self):
        self.run_install('--method', 'docker', '--version', 'v2.1.0-rc.1', '--no-start')
        self.assertIn('docker pull ghcr.io/x0rz4/easyproxiesv2:2.1.0-rc.1', self.calls.read_text())

    def test_unmanaged_container_is_not_removed(self):
        self.env['EXISTING_CONTAINER'] = '1'
        result = self.run_install('--method', 'docker', '--version', '2.1.0', ok=False)
        self.assertIn('not managed by this installer', result.stderr)
        self.assertNotIn('docker rm', self.calls.read_text())

    def test_invalid_version_and_architecture(self):
        self.run_install('--version', '../main', '--no-start', ok=False)
        self.env['TEST_ARCH'] = 'riscv64'
        self.run_install('--version', '2.1.0', '--no-start', ok=False)


if __name__ == '__main__':
    unittest.main()
