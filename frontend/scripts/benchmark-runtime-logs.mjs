// Local CPU microbenchmark, excluding SSE parsing, React, DOM and networking.
// Run from frontend: node scripts/benchmark-runtime-logs.mjs
import { execFileSync } from 'node:child_process'
import { mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { performance } from 'node:perf_hooks'
import assert from 'node:assert/strict'
import ts from 'typescript'
import { build, transform } from 'esbuild'

const root = fileURLToPath(new URL('..', import.meta.url))
const output = new URL('../../.tools/frontend-audit/log-benchmark.mjs', import.meta.url)
await mkdir(new URL('.', output), { recursive:true })
await build({ entryPoints:[fileURLToPath(new URL('../src/utils/runtimeLogs.ts', import.meta.url))],
  outfile:fileURLToPath(output), bundle:true, platform:'node', format:'esm', target:'node22' })
const { runtimeLog, createLogBuffer, mergeLogs } = await import(output.href)
const baseline = execFileSync('git', ['show','fdd9d08:frontend/src/components/DebugPanel.tsx'], {cwd:root, encoding:'utf8'})
const source = ts.createSourceFile('baseline.tsx', baseline, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
const helpers = source.statements.filter(statement => ts.isFunctionDeclaration(statement) && ['logKey','mergeLogs'].includes(statement.name?.text)).map(statement => statement.getText(source)).join('\n')
assert.match(helpers, /function mergeLogs/)
const compiled = await transform('const maxLogEntries=1000;\n'+helpers+'\nexport { mergeLogs }', {loader:'ts', format:'esm'})
const { mergeLogs:originalMerge } = await import('data:text/javascript;base64,'+Buffer.from(compiled.code).toString('base64'))
const median = values => [...values].sort((a,b)=>a-b)[1]
const results = []
for (const count of [1000,10000]) {
  const incoming = Array.from({length:count}, (_,id)=>({nodeTag:`node-${id}`,nodeName:'fixture',event:{time:new Date(1700000000000+id).toISOString(),success:true,latency_ms:1,destination:'fixture'}}))
  const variants = {
    original: () => {let logs=[]; for(const log of incoming) logs=originalMerge(logs,[log]); return logs},
    phase1: () => {let pending=[]; for(const log of incoming) {pending.push(log);if(pending.length>1000) pending=pending.slice(-1000)} return originalMerge([],pending)},
    ring: () => {const buffer=createLogBuffer();for(const log of incoming)buffer.push(runtimeLog(log.nodeTag,log.nodeName,log.event)); return mergeLogs([],buffer.drain())},
  }
  for (const [variant,run] of Object.entries(variants)) {
    const samples=[]
    for(let repeat=0;repeat<3;repeat++) {
      const start=performance.now(), logs=run(); samples.push(performance.now()-start)
      assert.equal(logs.length,1000);assert.equal(logs[0].nodeTag,`node-${count-1}`)
    }
    results.push({count,variant,median_ms:Number(median(samples).toFixed(3)),samples_ms:samples.map(value=>Number(value.toFixed(3)))})
  }
}
console.log(JSON.stringify({node:process.version,scope:'single synchronous burst; three runs; no SSE/React/DOM/network',results},null,2))
