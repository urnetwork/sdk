#!/usr/bin/env node
// Durable, serial fresh-process runner for the production native RPC benchmark.
// No packages required: Node >= 20 and the SDK's Go toolchain are sufficient.
// Run from any directory: node sdk/build/bench-device-rpc.mjs --pairs=10
// --memory-pairs=2 --gomaxprocs=3 --out=/new/artifact/directory
// Add --baseline-binary=/previous/artifact/sdk-device-rpc.test to compare a
// pinned pre-change XL arm in the same alternating fresh-process blocks.
// A memory arm is intentionally excluded from timing confidence intervals.

import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const sdk = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const sizes = [
  { bytes: 256, iterations: 12000 },
  { bytes: 1200, iterations: 12000 },
  { bytes: 65536, iterations: 2000 },
  { bytes: 3 * 1024 * 1024 - 4096, iterations: 120 },
];
const directions = ['forward', 'reverse', 'duplex'];
const bootstrapSeed = 0x481beef;
const bootstrapSamples = 20000;

function median(values) {
  assert(values.length > 0);
  const x = [...values].sort((a, b) => a - b);
  return (x[(x.length - 1) >> 1] + x[x.length >> 1]) / 2;
}

function pairedChange(pairs, field, reduction = false) {
  const changes = pairs.map(([ws, xl]) => {
    assert(Number.isFinite(ws[field]) && ws[field] > 0 && Number.isFinite(xl[field]));
    return 100 * (reduction ? 1 - xl[field] / ws[field] : xl[field] / ws[field] - 1);
  });
  let seed = bootstrapSeed;
  const next = () => {
    seed ^= seed << 13;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    return (seed >>> 0) / 2 ** 32;
  };
  const samples = [];
  for (let i = 0; i < bootstrapSamples; i++) {
    samples.push(median(changes.map(() => changes[Math.floor(next() * changes.length)])));
  }
  samples.sort((a, b) => a - b);
  return {
    median_percent: median(changes),
    bootstrap_95_percent: [samples[Math.floor(samples.length * 0.025)], samples[Math.floor(samples.length * 0.975)]],
    paired_changes_percent: changes,
  };
}

function summarize(records) {
  const summary = [];
  for (const size of sizes) {
    for (const direction of directions) {
      const matching = records.filter(r => r.payload_bytes === size.bytes && r.direction === direction);
      const timing = matching.filter(r => !r.memory_diagnostic);
      if (!timing.length) continue;
      const armOf = r => r.arm ?? r.carrier;
      const arms = ['websocket', 'framerxl'];
      if (timing.some(r => armOf(r) === 'framerxl-before')) arms.push('framerxl-before');
      const blocks = [...new Set(timing.map(r => r.block))];
      const pairFor = control => blocks.map(block => {
        const pair = [control, 'framerxl'].map(mode => timing.find(r => r.block === block && armOf(r) === mode));
        assert(pair.every(Boolean), `incomplete pair for ${size.bytes}/${direction}/${block}`);
        assert.equal(pair[0].carrier_frames_per_rpc, pair[1].carrier_frames_per_rpc, 'paired arms must send identical gob/mux frame counts');
        assert.equal(pair[0].carrier_payload_bytes_per_rpc, pair[1].carrier_payload_bytes_per_rpc, 'paired arms must send identical gob/mux payload bytes');
        return pair;
      });
      const pairs = pairFor('websocket');
      const entry = { payload_bytes: size.bytes, direction, pairs: pairs.length, modes: {} };
      for (const carrier of arms) {
        const arm = timing.filter(r => armOf(r) === carrier);
        const memory = matching.filter(r => armOf(r) === carrier && r.memory_diagnostic);
        const values = {};
        for (const key of [
          'latency_p50_ns', 'latency_p95_ns', 'latency_p99_ns', 'cpu_ns_per_rpc', 'payload_mbps',
          'allocations_per_rpc', 'allocated_bytes_per_rpc', 'carrier_frames_per_rpc',
          'carrier_payload_bytes_per_rpc', 'tls_socket_writes_per_rpc', 'tls_socket_bytes_per_rpc',
          'gc_count', 'gc_pause_ns',
        ]) values[key] = median(arm.map(r => r[key]));
        if (memory.length) {
          values.memory = {
            observations: memory.length,
            median_runtime_before_bytes: median(memory.map(r => r.before.runtime_bytes)),
            median_runtime_peak_bytes: median(memory.map(r => r.sampled_peak.runtime_bytes)),
            max_runtime_peak_bytes: Math.max(...memory.map(r => r.sampled_peak.runtime_bytes)),
            median_heap_peak_bytes: median(memory.map(r => r.sampled_peak.heap_bytes)),
            median_post_close_gc_runtime_bytes: median(memory.map(r => r.post_close_gc.runtime_bytes)),
            median_post_close_gc_heap_bytes: median(memory.map(r => r.post_close_gc.heap_bytes)),
            max_process_peak_rss_bytes: Math.max(...memory.map(r => r.process_peak_rss_bytes)),
          };
        }
        entry.modes[carrier] = values;
      }
      entry.changes = {
        throughput_improvement: pairedChange(pairs, 'payload_mbps'),
        cpu_reduction: pairedChange(pairs, 'cpu_ns_per_rpc', true),
        p50_latency_reduction: pairedChange(pairs, 'latency_p50_ns', true),
        p95_latency_reduction: pairedChange(pairs, 'latency_p95_ns', true),
        allocation_bytes_reduction: pairedChange(pairs, 'allocated_bytes_per_rpc', true),
      };
      if (arms.includes('framerxl-before')) {
        const baselinePairs = pairFor('framerxl-before');
        entry.collector_fix_changes = {
          throughput_improvement: pairedChange(baselinePairs, 'payload_mbps'),
          cpu_reduction: pairedChange(baselinePairs, 'cpu_ns_per_rpc', true),
          p50_latency_reduction: pairedChange(baselinePairs, 'latency_p50_ns', true),
          p95_latency_reduction: pairedChange(baselinePairs, 'latency_p95_ns', true),
          allocation_bytes_reduction: pairedChange(baselinePairs, 'allocated_bytes_per_rpc', true),
        };
      }
      summary.push(entry);
    }
  }
  return summary;
}

function run(command, args, opts = {}) {
  const r = spawnSync(command, args, { cwd: sdk, encoding: 'utf8', timeout: 180000, maxBuffer: 4 * 1024 * 1024, ...opts });
  if (r.error || r.status !== 0) throw new Error(`${command} ${args.join(' ')} failed: ${r.error ?? r.status}\n${r.stdout ?? ''}\n${r.stderr ?? ''}`);
  return r.stdout;
}

function sha256(filename) {
  return createHash('sha256').update(fs.readFileSync(filename)).digest('hex');
}

function selfTest() {
  assert.equal(median([3, 1, 2]), 2);
  assert.equal(median([1, 5, 3, 7]), 4);
  const equal = pairedChange([[{ v: 10 }, { v: 10 }], [{ v: 20 }, { v: 20 }]], 'v');
  assert.deepEqual(equal.bootstrap_95_percent, [0, 0]);
  assert.equal(equal.median_percent, 0);
  assert.equal(pairedChange([[{ v: 10 }, { v: 8 }]], 'v', true).median_percent.toFixed(6), '20.000000');
  assert.throws(() => pairedChange([[{ v: 0 }, { v: 10 }]], 'v'));
  console.log('Device RPC runner self-test passed');
}

function main() {
  const args = new Map(process.argv.slice(2).map(value => {
    const match = /^--([^=]+)(?:=(.*))?$/.exec(value);
    if (!match) throw new Error(`invalid argument ${value}`);
    return [match[1], match[2] ?? true];
  }));
  for (const name of args.keys()) {
    if (!['pairs', 'memory-pairs', 'gomaxprocs', 'out', 'smoke', 'self-test', 'analyze', 'baseline-binary'].includes(name)) throw new Error(`unknown option ${name}`);
  }
  if (args.has('self-test')) return selfTest();
  if (args.has('analyze')) {
    const records = fs.readFileSync(path.resolve(String(args.get('analyze'))), 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
    console.log(JSON.stringify(summarize(records), null, 2));
    return;
  }
  const integer = (name, fallback, low, high) => {
    const n = Number(args.get(name) ?? fallback);
    if (!Number.isInteger(n) || n < low || n > high) throw new Error(`--${name} must be an integer in [${low}, ${high}]`);
    return n;
  };
  const pairs = integer('pairs', 10, 1, 100);
  const memoryPairs = integer('memory-pairs', 2, 0, 100);
  const gomaxprocs = integer('gomaxprocs', 3, 1, Math.max(1, Math.ceil(os.availableParallelism() * 0.7)));
  const smoke = args.has('smoke');
  const baselineBinary = args.has('baseline-binary') ? path.resolve(String(args.get('baseline-binary'))) : null;
  const output = args.has('out') ? path.resolve(String(args.get('out'))) : fs.mkdtempSync(path.join(os.tmpdir(), 'urnetwork-device-rpc-'));
  if (args.has('out')) fs.mkdirSync(output, { recursive: false }); // refuse accidental overwrite
  const environment = { ...process.env, GOMAXPROCS: String(gomaxprocs) };
  const binary = path.join(output, 'sdk-device-rpc.test');
  const cases = sizes.flatMap(size => directions.filter(direction => direction !== 'duplex' || 2 * (size.bytes + 4096) <= 4 * 1024 * 1024).map(direction => ({
    payload_bytes: size.bytes,
    iterations: smoke ? 8 : size.iterations,
    direction,
  })));
  const provenance = {
    schema: 1, started_utc: new Date().toISOString(), pairs, memory_pairs: memoryPairs, smoke,
    go_version: run('go', ['version'], { env: environment }).trim(),
    gomaxprocs, host: { platform: os.platform(), arch: os.arch(), cpu: os.cpus()[0]?.model, cores: os.availableParallelism() },
    cases, bootstrap: { samples: bootstrapSamples, seed: bootstrapSeed, estimator: 'median of per-block paired percentage changes' },
    exclusions: ['handshake', '16 warmup RPCs per direction', 'setup', 'teardown'],
    negative_control: 'near-3MiB duplex exceeds unchanged 4MiB per-endpoint shared receive budget; deterministic real-mTLS test asserts terminal admission rejection and ownership cleanup for both carriers; not a passing throughput case',
    scope: 'both endpoints; pinned local mTLS; real native dialer/handler/deviceRpcMux/net-rpc gob; one outstanding call per stream; request+reply payload Mb/s; no added batching',
    memory: 'separate fresh-process arms; 1ms runtime/metrics sampling plus every completed call; sampled lower bound, not absolute iOS-profile gate',
    tls_writes: 'underlying encrypted socket Write calls (TLS record splitting included), not tls.Conn.Write calls',
    baseline_binary: baselineBinary ? { path: baselineBinary, sha256: sha256(baselineBinary) } : null,
    files_sha256: Object.fromEntries(['device_rpc_h1plus_benchmark_test.go', 'build/bench-device-rpc.mjs'].map(file => [file, sha256(path.join(sdk, file))])),
    repos: Object.fromEntries(['sdk', 'connect'].map(repo => [repo, {
      head: run('git', ['-C', path.join(sdk, '..', repo), 'rev-parse', 'HEAD']).trim(),
      status: run('git', ['-C', path.join(sdk, '..', repo), 'status', '--short']).trim(),
      diff_sha256: createHash('sha256').update(run('git', ['-C', path.join(sdk, '..', repo), 'diff', 'HEAD'])).digest('hex'),
    }])),
  };
  fs.writeFileSync(path.join(output, 'manifest.json'), JSON.stringify(provenance, null, 2) + '\n');
  console.log(`Building measurement binary; artifacts: ${output}`);
  run('go', ['test', '-c', '-o', binary, '.'], { env: environment });
  provenance.binary_sha256 = sha256(binary);
  fs.writeFileSync(path.join(output, 'manifest.json'), JSON.stringify(provenance, null, 2) + '\n');
  const observations = [];
  const raw = path.join(output, 'observations.jsonl');
  const orders = baselineBinary ? [
    ['websocket', 'framerxl-before', 'framerxl'],
    ['framerxl', 'websocket', 'framerxl-before'],
    ['framerxl-before', 'framerxl', 'websocket'],
    ['framerxl-before', 'websocket', 'framerxl'],
    ['framerxl', 'framerxl-before', 'websocket'],
    ['websocket', 'framerxl', 'framerxl-before'],
  ] : [['websocket', 'framerxl'], ['framerxl', 'websocket']];
  const total = (pairs + memoryPairs) * cases.length * orders[0].length;
  for (const [phase, blocks] of [['timing', pairs], ['memory', memoryPairs]]) {
    for (let block = 0; block < blocks; block++) {
      for (let offset = 0; offset < cases.length; offset++) {
        const index = (offset + block) % cases.length;
        const c = cases[index];
        const modes = orders[(block + index) % orders.length];
        for (let position = 0; position < modes.length; position++) {
          const mode = modes[position];
          const carrier = mode === 'websocket' ? mode : 'framerxl';
          const childEnv = {
            ...environment,
            DEVICE_RPC_BENCH_CARRIER: carrier,
            DEVICE_RPC_BENCH_PAYLOAD: String(c.payload_bytes),
            DEVICE_RPC_BENCH_ITERATIONS: String(c.iterations),
            DEVICE_RPC_BENCH_DIRECTION: c.direction,
            DEVICE_RPC_BENCH_MEMORY: phase === 'memory' ? '1' : '0',
          };
          let text;
          try {
            text = run(mode === 'framerxl-before' ? baselineBinary : binary, ['-test.run=^TestDeviceRpcH1PlusMeasurement$', '-test.count=1', '-test.timeout=150s'], { env: childEnv });
          } catch (error) {
            fs.appendFileSync(path.join(output, 'failures.jsonl'), JSON.stringify({
              phase, block, position, carrier: mode, ...c, failed_utc: new Date().toISOString(), error: String(error),
            }) + '\n');
            throw error; // never suppress a failed arm or average only survivors
          }
          const lines = text.split('\n').filter(line => line.startsWith('DEVICE_RPC_MEASUREMENT '));
          assert.equal(lines.length, 1, 'exactly one measurement is required per fresh process');
          const record = { block, position, arm: mode, ...JSON.parse(lines[0].slice('DEVICE_RPC_MEASUREMENT '.length)) };
          assert.equal(record.correct, true);
          assert.equal(record.carrier, carrier);
          assert.equal(record.direction, c.direction);
          assert.equal(record.payload_bytes, c.payload_bytes);
          assert.equal(record.iterations_per_direction, c.iterations);
          assert.equal(record.completed_rpcs, c.iterations * (c.direction === 'duplex' ? 2 : 1));
          assert.equal(record.gomaxprocs, gomaxprocs);
          assert.equal(record.memory_diagnostic, phase === 'memory');
          assert(record.carrier_frames_per_rpc >= 2 && record.tls_socket_writes_per_rpc > 0);
          if (phase === 'memory') assert(record.sample_count >= record.completed_rpcs);
          observations.push(record);
          fs.appendFileSync(raw, JSON.stringify(record) + '\n');
          console.log(`[${observations.length}/${total}] ${phase} pair ${block + 1} ${c.payload_bytes}/${c.direction}/${mode}: p50 ${(record.latency_p50_ns / 1e3).toFixed(1)} us, CPU ${(record.cpu_ns_per_rpc / 1e3).toFixed(1)} us/RPC, ${record.payload_mbps.toFixed(1)} Mb/s`);
        }
      }
    }
  }
  const result = { ...provenance, finished_utc: new Date().toISOString(), observation_count: observations.length, observations_sha256: sha256(raw), summary: summarize(observations) };
  fs.writeFileSync(path.join(output, 'results.json'), JSON.stringify(result, null, 2) + '\n');
  console.log(`Completed ${observations.length}/${total} verified observations. Summary: ${path.join(output, 'results.json')}`);
}

main();
