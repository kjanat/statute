import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cpSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const files = ['Cargo.toml', 'Cargo.lock', 'vendor/README.md'];

export function updatePins(manifest, version) {
	for (const name of ['lol_html', 'upstream_lol_html']) {
		const pattern = new RegExp(`^(${name}\\s*=\\s*\\{[^\\n]*version\\s*=\\s*")=[^"]+("[^\\n]*\\})$`, 'm');
		if (!pattern.test(manifest)) throw new Error(`Missing exact ${name} pin`);
		manifest = manifest.replace(pattern, `$1=${version}$2`);
	}
	return manifest;
}

export function verifyArchive(bytes, checksum) {
	if (!/^[a-f0-9]{64}$/.test(checksum) || createHash('sha256').update(bytes).digest('hex') !== checksum) {
		throw new Error('Archive checksum mismatch');
	}
}

function run(command, args, cwd) {
	return execFileSync(command, args, { cwd, encoding: 'utf8', timeout: 120000, maxBuffer: 32 << 20, stdio: 'pipe' });
}

function requireCleanTargets() {
	if (
		run('git', [
			'status',
			'--porcelain',
			'--',
			'guest/Cargo.toml',
			'guest/Cargo.lock',
			'guest/vendor/lol_html',
			'guest/vendor/README.md',
		], root).trim()
	) {
		throw new Error('Commit or stash guest pins/copied-source/provenance edits before refreshing');
	}
}

async function download(url) {
	const response = await fetch(url, {
		headers: { 'User-Agent': 'statute-lol-html-refresh (https://github.com/kjanat/statute)' },
		signal: AbortSignal.timeout(60000),
	});
	if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
	const bytes = Buffer.from(await response.arrayBuffer());
	if (bytes.length > 16 << 20) throw new Error('Download exceeds 16 MiB');
	return bytes;
}

// Preparation happens entirely in scratch space; callers can inspect it before
// replacing the tracked files. Cargo metadata resolves the lock without builds.
export function prepare(guest, scratch, version, archive, checksum) {
	verifyArchive(archive, checksum);
	const crate = `lol_html-${version}`;
	const tarball = join(scratch, 'upstream.crate');
	writeFileSync(tarball, archive);
	const names = run('tar', ['-tzf', tarball], scratch).trimEnd().split('\n');
	if (names.some(name => !name.startsWith(`${crate}/`) || !/^[\w./-]+$/.test(name) || name.split('/').includes('..'))) {
		throw new Error('Unexpected archive path');
	}
	if (run('tar', ['-tvzf', tarball], scratch).trimEnd().split('\n').some(line => !/^[-d]/.test(line))) {
		throw new Error('Archive contains links or special files');
	}
	run('tar', [
		'-xzf',
		tarball,
		'--no-same-owner',
		'--no-same-permissions',
		...[
			'src',
			'Cargo.toml',
			'Cargo.toml.orig',
			'LICENSE',
			'README.md',
			'.cargo_vcs_info.json',
		].map(path => `${crate}/${path}`),
	], scratch);
	const staged = join(scratch, 'guest');
	cpSync(join(guest, 'src'), join(staged, 'src'), { recursive: true });
	cpSync(join(scratch, crate), join(staged, 'vendor/lol_html'), { recursive: true });
	// Give git apply a local root even when scratch is inside the main checkout.
	run('git', ['init', '--quiet'], join(staged, 'vendor/lol_html'));
	run('git', ['apply', '--check', join(guest, 'vendor/end-token.patch')], join(staged, 'vendor/lol_html'));
	run('git', ['apply', join(guest, 'vendor/end-token.patch')], join(staged, 'vendor/lol_html'));
	rmSync(join(staged, 'vendor/lol_html/.git'), { recursive: true });
	writeFileSync(join(staged, 'Cargo.toml'), updatePins(readFileSync(join(guest, 'Cargo.toml'), 'utf8'), version));
	cpSync(join(guest, 'Cargo.lock'), join(staged, 'Cargo.lock'));
	let notes = readFileSync(join(guest, 'vendor/README.md'), 'utf8');
	if (!/`lol_html` \d+\.\d+\.\d+ archive/.test(notes) || !/`[a-f0-9]{64}`/.test(notes)) {
		throw new Error('Missing upstream provenance in vendor/README.md');
	}
	notes = notes.replace(/`lol_html` \d+\.\d+\.\d+ archive/, `\`lol_html\` ${version} archive`)
		.replace(/`[a-f0-9]{64}`/, `\`${checksum}\``);
	writeFileSync(join(staged, 'vendor/README.md'), notes);
	run('cargo', ['metadata', '--format-version', '1', '--manifest-path', join(staged, 'Cargo.toml')], root);
	return staged;
}

async function main(version) {
	if (!/^\d+\.\d+\.\d+$/.test(version ?? '')) throw new Error('Usage: node update-lol-html.mjs VERSION');
	const guest = join(root, 'guest');
	requireCleanTargets();
	console.log(`Fetching LOL HTML ${version}…`);
	const index = (await download('https://index.crates.io/lo/l_/lol_html')).toString('utf8');
	const metadata = index.trim().split('\n').map(line => JSON.parse(line)).find(entry => entry.vers === version);
	if (!metadata || metadata.name !== 'lol_html' || metadata.yanked) throw new Error('Missing or yanked release');
	const archive = await download(`https://static.crates.io/crates/lol_html/lol_html-${version}.crate`);
	const scratch = mkdtempSync(join(guest, '.refresh-lol-html-'));
	let preserve = false;
	try {
		const staged = prepare(guest, scratch, version, archive, metadata.cksum);
		requireCleanTargets();
		const before = files.map(file => readFileSync(join(guest, file)));
		const target = join(guest, 'vendor/lol_html');
		const backup = join(scratch, 'previous');
		renameSync(target, backup);
		try {
			renameSync(join(staged, 'vendor/lol_html'), target);
			for (const file of files) cpSync(join(staged, file), join(guest, file));
		} catch (error) {
			preserve = true;
			rmSync(target, { recursive: true, force: true });
			renameSync(backup, target);
			files.forEach((file, i) => writeFileSync(join(guest, file), before[i]));
			preserve = false;
			throw error;
		}
		console.log(`Updated LOL HTML to ${version}. Review git diff, then run make test.`);
	} finally {
		if (preserve) console.error(`Recovery files retained at ${scratch}`);
		else rmSync(scratch, { recursive: true, force: true });
	}
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	main(process.argv[2]).catch(error => {
		console.error(`LOL HTML refresh failed: ${error.message}`);
		process.exitCode = 1;
	});
}
