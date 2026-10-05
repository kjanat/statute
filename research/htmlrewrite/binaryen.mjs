import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync, copyFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { optimizerFlags, variants } from './binaryen-config.mjs';

process.chdir(dirname(fileURLToPath(import.meta.url)));
const root = resolve('artifact/binaryen');
mkdirSync(root, { recursive: true });
const phase = process.argv[2];
if (!['build', 'test', 'bench', 'load'].includes(phase)) {
	throw new Error('Usage: node binaryen.mjs build|test|bench|load');
}
const log = resolve(root, `${phase}.txt`);
writeFileSync(log, '');
function run(command, args, timeout = 600000) {
	const line = `$ ${command} ${args.map(arg => JSON.stringify(arg)).join(' ')}\n`;
	process.stdout.write(line);
	appendFileSync(log, line);
	const result = spawnSync(command, args, {
		encoding: 'utf8',
		timeout,
		maxBuffer: 64 << 20,
		env: { ...process.env, CGO_ENABLED: '0' },
	});
	const output = (result.stdout ?? '') + (result.stderr ?? '');
	process.stdout.write(output);
	appendFileSync(log, output + `\nExit: ${result.status}\n`);
	if (result.error || result.status !== 0) {
		throw result.error ?? new Error(`${command} failed: ${result.status}`);
	}
	return output;
}
function identity(name) {
	const bytes = readFileSync(resolve(root, `${name}.wasm`));
	const value = { variant: name, bytes: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') };
	const line = JSON.stringify(value) + '\n';
	process.stdout.write(line);
	appendFileSync(log, line);
	return value;
}

if (phase === 'build') {
	const tool = process.env.WASM_OPT ?? 'wasm-opt';
	if (!/^wasm-opt version 133\b/m.test(run(tool, ['--version']))) {
		throw new Error('This comparison requires Binaryen 133');
	}
	run('rustc', ['--version', '--verbose']);
	run('go', ['version']);
	run('make', ['build', 'WASM_RUSTFLAGS=']);
	copyFileSync('artifact/rewriter.wasm', resolve(root, 'baseline.wasm'));
	const manifest = [];
	for (const [name, flags] of variants) {
		const wasm = resolve(root, `${name}.wasm`);
		const effectiveFlags = optimizerFlags(name, flags);
		if (name !== 'baseline') run(tool, [resolve(root, 'baseline.wasm'), ...effectiveFlags, '-o', wasm]);
		const record = identity(name);
		manifest.push({ ...record, flags: effectiveFlags });
		run(tool, [wasm, ...optimizerFlags('metrics', []), '--metrics']);
		const overlay = resolve(root, `${name}.json`);
		writeFileSync(overlay, JSON.stringify({ Replace: { [resolve('artifact/rewriter.wasm')]: wasm } }));
		run('go', [
			'test',
			`-overlay=${overlay}`,
			'-tags=statute_htmlrewrite,htmlrewrite_research',
			'-c',
			'-o',
			resolve(root, `${name}.test`),
		]);
	}
	writeFileSync(resolve(root, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
} else if (phase === 'test') {
	for (const [name] of variants) {
		const record = identity(name);
		const output = run(resolve(root, `${name}.test`), ['-test.v', '-test.timeout=5m']);
		if (!output.includes(`optimizer_guest sha256=${record.sha256}`)) {
			throw new Error(`Embedded artifact mismatch: ${name}`);
		}
	}
} else if (phase === 'bench') {
	// Rotate the order across independent processes to reduce order bias.
	for (let round = 0; round < 3; round++) {
		for (let index = 0; index < variants.length; index++) {
			const [name] = variants[(index + round * 2) % variants.length];
			identity(name);
			run(resolve(root, `${name}.test`), [
				'-test.run=^$',
				'-test.bench=^(BenchmarkOptimizer|BenchmarkInstance|BenchmarkColdEngine|BenchmarkFirstOutput)$',
				'-test.benchmem',
				'-test.benchtime=500ms',
				'-test.timeout=5m',
			]);
		}
	}
} else {
	for (const [name] of variants) {
		identity(name);
		for (const mode of ['stream', 'etag']) {
			run(resolve(root, `${name}.test`), [
				'-test.run=^TestHTTPLoad$',
				'-test.v',
				'-test.timeout=2m',
				'-http-load-duration=3s',
				'-http-load-count=131072',
				'-http-load-workers=4',
				`-http-load-mode=${mode}`,
				'-http-load-pace=16ms',
			]);
		}
		run(resolve(root, `${name}.test`), [
			'-test.run=^TestMemoryLoad$',
			'-test.v',
			'-test.timeout=2m',
			'-load-duration=3s',
			'-load-rounds=3',
			'-load-workers=4',
			'-load-count=32768',
		]);
	}
}
