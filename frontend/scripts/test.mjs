import { readdir, mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { build } from 'esbuild'

const root = fileURLToPath(new URL('..', import.meta.url))
const output = fileURLToPath(new URL('../../.tools/frontend-audit/tests/', import.meta.url))
await mkdir(output, { recursive: true })
const tests = (await readdir(join(root, 'tests'))).filter(name => name.endsWith('.test.mjs'))
await build({ entryPoints: tests.map(name => join(root, 'tests', name)), outdir: output,
  outExtension: { '.js': '.mjs' }, bundle: true, platform: 'node', format: 'esm', target: 'node22', sourcemap: 'inline' })
const result = spawnSync(process.execPath, ['--test', '--test-timeout=10000', ...tests.map(name => join(output, name))], { stdio: 'inherit' })
process.exitCode = result.status ?? 1
