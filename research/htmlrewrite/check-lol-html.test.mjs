import assert from 'node:assert/strict';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { checkConsistency, targetVersion } from './check-lol-html.mjs';

const guest = join(dirname(fileURLToPath(import.meta.url)), 'guest');

function fixture(t) {
	const root = mkdtempSync(join(tmpdir(), 'statute-lol-html-check-'));
	t.after(() => rmSync(root, { recursive: true, force: true }));
	for (const path of ['Cargo.toml', 'Cargo.lock', 'vendor/README.md', 'vendor/lol_html/Cargo.toml']) {
		mkdirSync(dirname(join(root, path)), { recursive: true });
		cpSync(join(guest, path), join(root, path));
	}
	return root;
}

test('checked-in source, both pins, lockfile and provenance agree', () => {
	assert.equal(checkConsistency(), targetVersion());
});

test('a registry-only Renovate update fails before Cargo runs', t => {
	const root = fixture(t);
	const manifest = join(root, 'Cargo.toml');
	writeFileSync(
		manifest,
		readFileSync(manifest, 'utf8').replace(/(upstream_lol_html[^\n]*version = ")=[^"]+/, '$1=99.0.0'),
	);
	assert.equal(targetVersion(root), '99.0.0');
	assert.throws(() => checkConsistency(root), /LOL HTML versions differ:.*upstream_lol_html=99\.0\.0/);
});

for (const path of ['vendor/lol_html/Cargo.toml', 'vendor/README.md', 'Cargo.lock']) {
	test(`drift in ${path} fails the consistency gate`, t => {
		const root = fixture(t);
		const file = join(root, path);
		writeFileSync(file, readFileSync(file, 'utf8').replace(targetVersion(root), '99.0.0'));
		assert.throws(() => checkConsistency(root), /LOL HTML versions differ/);
	});
}

test('the archive checksum in provenance must match the registry lock entry', t => {
	const root = fixture(t);
	const file = join(root, 'vendor/README.md');
	writeFileSync(file, readFileSync(file, 'utf8').replace(/`[a-f0-9]{64}`/, `\`${'0'.repeat(64)}\``));
	assert.throws(() => checkConsistency(root), /checksum differs/);
});
