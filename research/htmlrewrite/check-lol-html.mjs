import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const guest = join(dirname(fileURLToPath(import.meta.url)), 'guest');

function match(text, pattern, label) {
	const result = text.match(pattern);
	if (!result) throw new Error(`Missing ${label}`);
	return result[1];
}

export function targetVersion(directory = guest) {
	return manifestVersion(readFileSync(join(directory, 'Cargo.toml'), 'utf8'));
}

function manifestVersion(manifest) {
	return match(
		manifest,
		/^upstream_lol_html\s*=\s*\{[^\n]*version\s*=\s*"=(\d+\.\d+\.\d+)"[^\n]*\}$/m,
		'exact upstream_lol_html pin',
	);
}

export function checkConsistency(directory = guest) {
	return checkFiles(path => readFileSync(join(directory, path), 'utf8'));
}

export function checkFiles(read) {
	const manifest = read('Cargo.toml');
	const version = manifestVersion(manifest);
	const notes = read('vendor/README.md');
	const versions = {
		upstream_lol_html: version,
		lol_html: match(manifest, /^lol_html\s*=\s*\{[^\n]*version\s*=\s*"=([^"]+)"/m, 'local lol_html pin'),
		'vendor/lol_html': match(read('vendor/lol_html/Cargo.toml'), /^version\s*=\s*"([^"]+)"/m, 'vendored version'),
		'vendor/README.md': match(notes, /`lol_html` (\d+\.\d+\.\d+) archive/, 'provenance version'),
	};
	const packages = read('Cargo.lock').split(/^\[\[package\]\]/m)
		.filter(block => /^name = "lol_html"$/m.test(block));
	if (packages.length !== 2) throw new Error('Expected local and registry lol_html packages in Cargo.lock');
	let registryChecksum;
	for (const block of packages) {
		const registry = /^source = "registry\+/m.test(block);
		const label = `Cargo.lock (${registry ? 'registry' : 'local'})`;
		if (label in versions) throw new Error('Expected one local and one registry lol_html package in Cargo.lock');
		versions[label] = match(block, /^version = "([^"]+)"/m, 'locked version');
		if (registry) {
			registryChecksum = match(block, /^checksum = "([a-f0-9]{64})"$/m, 'registry checksum');
		}
	}
	if (Object.values(versions).some(value => value !== version)) {
		throw new Error(
			`LOL HTML versions differ: ${
				Object.entries(versions).map(([name, value]) => `${name}=${value}`).join(', ')
			}. Run make update-lol-html VERSION=${version}`,
		);
	}
	if (registryChecksum !== match(notes, /`([a-f0-9]{64})`/, 'provenance checksum')) {
		throw new Error('LOL HTML provenance checksum differs from Cargo.lock');
	}
	return version;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	try {
		console.log(
			process.argv[2] === '--target-version'
				? targetVersion()
				: `LOL HTML ${checkConsistency()}: source, pins, lockfile and provenance agree`,
		);
	} catch (error) {
		console.error(error.message);
		process.exitCode = 1;
	}
}
