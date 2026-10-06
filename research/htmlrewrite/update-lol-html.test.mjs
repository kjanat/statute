import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cpSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { prepare, updatePins, verifyArchive } from './update-lol-html.mjs';

const root = dirname(fileURLToPath(import.meta.url));

test('both pins change together without changing other versions', () => {
	const manifest = readFileSync(join(root, 'guest/Cargo.toml'), 'utf8');
	const updated = updatePins(manifest, '3.2.0');
	assert.equal((updated.match(/=3\.2\.0/g) ?? []).length, 2);
	assert.match(updated, /version\s*= "0\.0\.0"/);
	assert.throws(() => updatePins(manifest.replace('upstream_lol_html', 'missing'), '3.2.0'), /Missing exact/);
});

test('bad checksum is rejected', () => {
	assert.throws(() => verifyArchive(Buffer.from('wrong'), '0'.repeat(64)), /checksum mismatch/);
});

test('refresh reconstructs the patched source and leaves checkout untouched on conflict', () => {
	const dir = mkdtempSync(join(tmpdir(), 'statute-lol-html-test-'));
	const oldOffline = process.env.CARGO_NET_OFFLINE;
	process.env.CARGO_NET_OFFLINE = 'true';
	try {
		const guest = join(dir, 'original');
		for (const file of ['Cargo.toml', 'Cargo.lock', 'src', 'vendor']) {
			cpSync(join(root, 'guest', file), join(guest, file), { recursive: true });
		}
		const version = readFileSync(join(guest, 'Cargo.toml'), 'utf8').match(/version = "=(\d+\.\d+\.\d+)"/)[1];
		const upstream = join(dir, `lol_html-${version}`);
		cpSync(join(guest, 'vendor/lol_html'), upstream, { recursive: true });
		execFileSync('git', ['init', '--quiet'], { cwd: upstream });
		execFileSync('git', ['apply', '--reverse', join(guest, 'vendor/end-token.patch')], { cwd: upstream });
		rmSync(join(upstream, '.git'), { recursive: true });
		const archive = execFileSync('tar', ['-czf', '-', `lol_html-${version}`], { cwd: dir });
		const checksum = createHash('sha256').update(archive).digest('hex');
		const before = readFileSync(join(guest, 'Cargo.toml'));
		const scratch = mkdtempSync(join(dir, 'prepare-'));
		const staged = prepare(guest, scratch, version, archive, checksum);
		execFileSync('diff', ['-r', join(guest, 'vendor/lol_html'), join(staged, 'vendor/lol_html')]);
		assert.deepEqual(readFileSync(join(staged, 'Cargo.toml')), before);
		assert.deepEqual(readFileSync(join(staged, 'Cargo.lock')), readFileSync(join(guest, 'Cargo.lock')));
		assert.ok(readFileSync(join(staged, 'vendor/README.md'), 'utf8').includes(checksum));
		symlinkSync('../../../outside', join(upstream, 'src/escape'));
		const linkedArchive = execFileSync('tar', ['-czf', '-', `lol_html-${version}`], { cwd: dir });
		assert.throws(
			() =>
				prepare(
					guest,
					mkdtempSync(join(dir, 'links-')),
					version,
					linkedArchive,
					createHash('sha256').update(linkedArchive).digest('hex'),
				),
			/links or special files/,
		);
		writeFileSync(join(guest, 'vendor/end-token.patch'), 'broken patch\n');
		assert.throws(() => prepare(guest, mkdtempSync(join(dir, 'conflict-')), version, archive, checksum));
		assert.deepEqual(readFileSync(join(guest, 'Cargo.toml')), before);
		execFileSync('diff', ['-r', join(root, 'guest/vendor/lol_html'), join(guest, 'vendor/lol_html')]);
	} finally {
		if (oldOffline === undefined) delete process.env.CARGO_NET_OFFLINE;
		else process.env.CARGO_NET_OFFLINE = oldOffline;
		rmSync(dir, { recursive: true, force: true });
	}
});
