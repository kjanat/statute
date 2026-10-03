#!/usr/bin/env node
// @ts-check
// Adapted from micro509's RFC fetch/provenance approach; see docs/rfc-sources.md.
import { createHash, randomUUID } from 'node:crypto';
import { mkdir, readdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const directory = fileURLToPath(new URL('../docs/rfc/', import.meta.url));
const maxBytes = 10 * 1024 * 1024;

/** @typedef {{number: number, url: string, file: string, sha256: string, bytes: number, fetchedAt: string}} Entry */
/** @typedef {{version: 1, documents: Entry[]}} Manifest */

/** @param {string} value */
export function rfcNumber(value) {
	if (!/^[1-9][0-9]*$/.test(value) || !Number.isSafeInteger(Number(value))) {
		throw new Error(`Invalid RFC number: ${value}`);
	}
	return Number(value);
}

/** @param {number} number */
function sourceURL(number) {
	return `https://www.rfc-editor.org/rfc/rfc${number}.html`;
}

/** @param {Uint8Array} bytes */
function digest(bytes) {
	return createHash('sha256').update(bytes).digest('hex');
}

/** @param {string} dir @returns {Promise<Manifest>} */
async function readManifest(dir) {
	/** @type {Manifest} */
	const manifest = JSON.parse(await readFile(join(dir, 'manifest.json'), 'utf8'));
	if (manifest?.version !== 1 || !Array.isArray(manifest.documents)) {
		throw new Error('Invalid RFC manifest');
	}
	const numbers = new Set();
	for (const entry of manifest.documents) {
		if (
			!entry || !Number.isSafeInteger(entry.number) || entry.number < 1 || numbers.has(entry.number)
			|| entry.file !== `rfc${entry.number}.html` || entry.url !== sourceURL(entry.number)
			|| typeof entry.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(entry.sha256)
			|| !Number.isSafeInteger(entry.bytes) || entry.bytes < 1 || entry.bytes > maxBytes
			|| typeof entry.fetchedAt !== 'string' || !Number.isFinite(Date.parse(entry.fetchedAt))
		) throw new Error('Invalid or duplicate RFC manifest entry');
		numbers.add(entry.number);
	}
	return manifest;
}

/** @param {Uint8Array} bytes @param {number} number */
function validateHTML(bytes, number) {
	const html = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
	// Reject error/challenge pages, wrong RFCs, and visibly truncated downloads.
	// Sanitization and document authentication are outside this sanity check.
	const modern = /^\s*<!doctype html\s*>/i.test(html)
		&& new RegExp(`<title>\\s*RFC ${number}(?=[:\\s<])[^<]*</title>`, 'i').test(html)
		&& /<\/html>\s*$/i.test(html);
	// Older RFC Editor HTML uses annotated <pre> fragments.
	const legacy = /^\s*<pre>/i.test(html)
		&& new RegExp(`^Request for Comments: ${number}(?:\\s|$)`, 'm').test(html.slice(0, 4096))
		&& /<\/pre>\s*$/i.test(html);
	if (!modern && !legacy) throw new Error(`RFC ${number}: response is not the expected complete HTML document`);
}

/** @param {string} dir */
export async function check(dir = directory) {
	const manifest = await readManifest(dir);
	for (const entry of manifest.documents) {
		const bytes = await readFile(join(dir, entry.file));
		if (bytes.length !== entry.bytes || digest(bytes) !== entry.sha256) {
			throw new Error(`${entry.file}: snapshot differs from manifest; inspect changes before refreshing`);
		}
		validateHTML(bytes, entry.number);
	}
	const expected = new Set(manifest.documents.map((entry) => entry.file));
	for (const file of await readdir(dir)) {
		if (file.endsWith('.html') && !expected.has(file)) {
			throw new Error(`${file}: HTML snapshot is missing from manifest`);
		}
	}
	return manifest;
}

/** @param {number} number @param {typeof fetch} fetcher */
async function download(number, fetcher) {
	const url = sourceURL(number);
	const response = await fetcher(url, { signal: AbortSignal.timeout(30_000), redirect: 'error' });
	if (!response.ok || response.status !== 200) {
		await response.body?.cancel();
		throw new Error(`${url}: HTTP ${response.status}`);
	}
	if (!/^text\/html(?:\s*;|$)/i.test(response.headers.get('content-type') ?? '')) {
		await response.body?.cancel();
		throw new Error(`${url}: expected text/html`);
	}
	if (!response.body) throw new Error(`${url}: empty response`);
	const reader = response.body.getReader();
	const chunks = [];
	let length = 0;
	try {
		for (;;) {
			const { value, done } = await reader.read();
			if (done) break;
			length += value.length;
			if (length > maxBytes) throw new Error(`${url}: exceeds ${maxBytes} byte limit`);
			chunks.push(value);
		}
	} finally {
		try {
			await reader.cancel();
		} finally {
			reader.releaseLock();
		}
	}
	const bytes = Buffer.concat(chunks);
	validateHTML(bytes, number);
	return bytes;
}

/** @param {string} file @param {Uint8Array | string} body */
async function atomicWrite(file, body) {
	const temporary = `${file}.${randomUUID()}.tmp`;
	try {
		await writeFile(temporary, body, { flag: 'wx' });
		await rename(temporary, file);
	} finally {
		await rm(temporary, { force: true });
	}
}

/** @param {string[]} values @param {string} dir @param {typeof fetch} fetcher */
export async function refresh(values, dir = directory, fetcher = fetch) {
	const requested = values.map(rfcNumber);
	const manifest = await check(dir);
	const numbers = [...new Set(requested.length ? requested : manifest.documents.map((entry) => entry.number))];
	if (!numbers.length) throw new Error('No RFCs selected; pass RFC numbers to fetch');
	const entries = new Map(manifest.documents.map((entry) => [entry.number, entry]));
	const pending = [];
	// Download and validate the entire selection before replacing any snapshot.
	for (const number of numbers.sort((a, b) => a - b)) {
		const bytes = await download(number, fetcher);
		const sha256 = digest(bytes);
		const previous = entries.get(number);
		if (previous?.sha256 === sha256) continue;
		const entry = {
			number,
			url: sourceURL(number),
			file: `rfc${number}.html`,
			sha256,
			bytes: bytes.length,
			fetchedAt: new Date().toISOString(),
		};
		pending.push({ entry, bytes });
		entries.set(number, entry);
	}
	await mkdir(dir, { recursive: true });
	for (const { entry, bytes } of pending) await atomicWrite(join(dir, entry.file), bytes);
	if (pending.length) {
		const updated = { version: 1, documents: [...entries.values()].sort((a, b) => a.number - b.number) };
		await atomicWrite(join(dir, 'manifest.json'), `${JSON.stringify(updated, null, 2)}\n`);
	}
	return { checked: numbers.length, changed: pending.length };
}

/** @param {string[]} args */
async function main(args) {
	const [command, ...numbers] = args;
	if (command === '--help' || command === undefined) {
		console.log('Usage: node scripts/vendor-rfcs.mjs check | fetch [RFC ...]');
	} else if (command === 'check' && numbers.length === 0) {
		console.log(`Verified ${(await check()).documents.length} RFC HTML snapshots (offline).`);
	} else if (command === 'fetch') {
		const result = await refresh(numbers);
		console.log(
			`Fetched ${result.checked} RFCs; ${result.changed} snapshot(s) changed. Review docs/rfc/ before committing.`,
		);
	} else {
		throw new Error('Usage: node scripts/vendor-rfcs.mjs check | fetch [RFC ...]');
	}
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
	try {
		await main(process.argv.slice(2));
	} catch (error) {
		console.error(error instanceof Error ? error.message : String(error));
		process.exitCode = 1;
	}
}
