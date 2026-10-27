import test from 'node:test'
import assert from 'node:assert/strict'
import { createLogBuffer, maxLogEntries, mergeLogs, runtimeLog } from '../src/utils/runtimeLogs.ts'

const entry = (id, offset=id) => runtimeLog(`node-${id}`, 'fixture', { time: new Date(1700000000000+offset).toISOString(), success:true, latency_ms:1, destination:'fixture', error:'' })

test('bursts retain only the newest arrivals, drain in order and do not replay old slots', () => {
  const buffer = createLogBuffer()
  for (let i=0; i<maxLogEntries*10+7; i++) buffer.push(entry(i))
  const drained = buffer.drain()
  assert.equal(drained.length,maxLogEntries)
  assert.equal(drained[0].nodeTag,`node-${maxLogEntries*9+7}`)
  assert.equal(drained.at(-1).nodeTag,`node-${maxLogEntries*10+6}`)
  assert.deepEqual(buffer.drain(),[])
  const fresh=entry('fresh',1)
  buffer.push(fresh)
  assert.deepEqual(buffer.drain(),[fresh])
})

test('history and live updates deduplicate by event, retain unchanged rows and sort unknown time last', () => {
  const old=entry(1), recent=entry(2), latest=entry(3)
  const malformed=runtimeLog('unknown','fixture',{...old.event,time:'invalid'})
  const merged=mergeLogs([old,recent],[latest,malformed,recent])
  assert.deepEqual(merged.map(log=>log.nodeTag),['node-3','node-2','node-1','unknown'])
  assert.equal(merged[1],recent)
  const many=Array.from({length:maxLogEntries*2},(_,i)=>entry(i))
  assert.equal(mergeLogs([],many).length,maxLogEntries)
  assert.equal(mergeLogs([],many).at(-1).nodeTag,'node-1000')
})
