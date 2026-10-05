import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const cwd = dirname(fileURLToPath(import.meta.url));
function go(args) {
	return spawnSync('go', args, {
		cwd,
		encoding: 'utf8',
		timeout: 120000,
		maxBuffer: 32 << 20,
		env: { ...process.env, CGO_ENABLED: '0', GOFLAGS: '' },
	});
}
function success(args) {
	const result = go(args);
	assert.ifError(result.error);
	assert.equal(result.status, 0, `${args.join(' ')}\n${result.stdout}\n${result.stderr}`);
	return result.stdout;
}

test('feature and private-hook tags independently control linked engine availability', () => {
	const dir = mkdtempSync(join(tmpdir(), 'statute-rewrite-feature-'));
	try {
		const artifact = resolve(cwd, 'artifact/rewriter.wasm');
		const guest = readFileSync(artifact);
		const overlay = join(dir, 'without-guest.json');
		writeFileSync(overlay, JSON.stringify({ Replace: { [artifact]: '' } }));
		for (const enabled of [false, true]) {
			for (const hook of [false, true]) {
				const tags = [enabled && 'statute_htmlrewrite', hook && 'htmlrewrite_research'].filter(Boolean).join(',');
				const flags = [`-tags=${tags}`, ...enabled ? [] : [`-overlay=${overlay}`]];
				const info = JSON.parse(success(['list', ...flags, '-json', '.']));
				assert.equal((info.EmbedFiles ?? []).includes('artifact/rewriter.wasm'), enabled);
				const deps = success(['list', ...flags, '-deps', '.']);
				assert.equal(deps.includes('github.com/tetratelabs/wazero'), enabled);
				const binary = join(dir, `${enabled}-${hook}.test`);
				success(['test', ...flags, '-c', '-ldflags=-w', '-o', binary, '.']);
				const symbols = success(['tool', 'nm', binary]);
				assert.equal(symbols.includes('github.com/tetratelabs/wazero'), enabled);
				assert.equal(readFileSync(binary).includes(guest), enabled);
			}
		}
		const missing = go([
			'test',
			'-tags=statute_htmlrewrite',
			`-overlay=${overlay}`,
			'-c',
			'-o',
			join(dir, 'missing.test'),
			'.',
		]);
		assert.ifError(missing.error);
		assert.notEqual(missing.status, 0);
		assert.match(missing.stderr, /rewriter\.wasm|no matching files/);
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
});
