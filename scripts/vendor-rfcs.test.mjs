// @ts-check
import assert from 'node:assert/strict';
import { mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { check, refresh, rfcNumber } from './vendor-rfcs.mjs';

/** @param {import('node:test').TestContext} t */
async function corpus(t) {
	const dir = await mkdtemp(join(tmpdir(), 'statute-rfcs-'));
	t.after(() => rm(dir, { recursive: true, force: true }));
	await writeFile(join(dir, 'manifest.json'), '{"version":1,"documents":[]}\n');
	return dir;
}

/** @param {number} number @param {string} suffix */
function html(number = 9110, suffix = '') {
	return `<!DOCTYPE html>\r\n<html><head><title>RFC ${number}: Fixture</title></head><body><section id="section-12.5.3">${suffix}© Rights retained.</section></body></html>\r\n`;
}

/** @param {string} body @param {ResponseInit} options */
function response(body = html(), options = {}) {
	return new Response(body, { headers: { 'content-type': 'text/html; charset=utf-8' }, ...options });
}

test('fetch preserves raw bytes/anchors/notices; provenance and unchanged refresh are stable', async (t) => {
	const dir = await corpus(t);
	const body = html();
	let requests = 0;
	/** @type {typeof fetch} */
	const fetcher = async (url, options) => {
		requests++;
		assert.equal(url, 'https://www.rfc-editor.org/rfc/rfc9110.html');
		assert.equal(options?.redirect, 'error');
		assert.ok(options?.signal instanceof AbortSignal);
		return response(body);
	};
	assert.deepEqual(await refresh(['9110', '9110'], dir, fetcher), { checked: 1, changed: 1 });
	assert.equal(await readFile(join(dir, 'rfc9110.html'), 'utf8'), body);
	const original = await readFile(join(dir, 'manifest.json'), 'utf8');
	const manifest = await check(dir);
	assert.equal(manifest.documents[0].bytes, Buffer.byteLength(body));
	assert.equal(manifest.documents[0].url, 'https://www.rfc-editor.org/rfc/rfc9110.html');
	assert.equal(manifest.documents[0].sha256.length, 64);
	assert.deepEqual(await refresh([], dir, fetcher), { checked: 1, changed: 0 });
	assert.equal(requests, 2);
	assert.equal(await readFile(join(dir, 'manifest.json'), 'utf8'), original);
	assert.deepEqual((await readdir(dir)).sort(), ['manifest.json', 'rfc9110.html']);
});

test('a failed batch preserves all earlier snapshots and their manifest', async (t) => {
	const dir = await corpus(t);
	await refresh(['9110'], dir, async () => response());
	const original = await readFile(join(dir, 'manifest.json'), 'utf8');
	await assert.rejects(
		refresh(['9110', '9111'], dir, async (url) => {
			return String(url).includes('9110') ? response(html(9110, 'changed')) : response('unavailable', { status: 503 });
		}),
		/HTTP 503/,
	);
	assert.equal(await readFile(join(dir, 'rfc9110.html'), 'utf8'), html());
	assert.equal(await readFile(join(dir, 'manifest.json'), 'utf8'), original);
	assert.deepEqual((await readdir(dir)).sort(), ['manifest.json', 'rfc9110.html']);
	await check(dir);
});

test('legacy RFC Editor annotated HTML is retained without conversion', async (t) => {
	const dir = await corpus(t);
	const body =
		'<pre>Network Working Group\nRequest for Comments: 1952\n<span class="h1">GZIP file format</span>\n</pre>\n';
	await refresh(['1952'], dir, async () => response(body));
	await check(dir);
	assert.equal(await readFile(join(dir, 'rfc1952.html'), 'utf8'), body);
	await assert.rejects(refresh(['1953'], dir, async () => response(body)), /expected complete HTML/);
});

test('changed downloads update both bytes and checksum', async (t) => {
	const dir = await corpus(t);
	await refresh(['9110'], dir, async () => response());
	const old = (await check(dir)).documents[0].sha256;
	assert.deepEqual(await refresh(['9110'], dir, async () => response(html(9110, 'new'))), { checked: 1, changed: 1 });
	assert.notEqual((await check(dir)).documents[0].sha256, old);
});

test('stream failures preserve the corpus and release the response reader', async (t) => {
	const dir = await corpus(t);
	await refresh(['9110'], dir, async () => response());
	const body = new ReadableStream({
		start(controller) {
			controller.error(new Error('connection lost'));
		},
	});
	await assert.rejects(
		refresh(['9110'], dir, async () =>
			new Response(body, {
				headers: { 'content-type': 'text/html' },
			})),
		/connection lost/,
	);
	assert.equal(body.locked, false);
	await check(dir);
});

test('invalid IDs fail before network or filesystem changes', async (t) => {
	const dir = await corpus(t);
	for (const value of ['../9110', '0', '-1', '9.1', '1e3', '09110', 'NaN', '9007199254740992']) {
		assert.throws(() => rfcNumber(value), /Invalid RFC number/);
		await assert.rejects(refresh([value], dir, async () => assert.fail('must not fetch')), /Invalid RFC number/);
	}
	assert.equal(rfcNumber('9110'), 9110);
	await assert.rejects(refresh([], dir), /No RFCs selected/);
});

test('unexpected, truncated, wrong-document and oversized bodies fail without publication', async (t) => {
	const dir = await corpus(t);
	const cases = [
		{
			name: 'not HTML',
			make: () => response('{}', { headers: { 'content-type': 'application/json' } }),
			error: /expected text\/html/,
		},
		{
			name: 'challenge',
			make: () => response('<!DOCTYPE html><html><title>Challenge</title></html>'),
			error: /expected complete HTML/,
		},
		{ name: 'wrong RFC', make: () => response(html(9111)), error: /expected complete HTML/ },
		{ name: 'truncated', make: () => response(html().replace('</html>', '')), error: /expected complete HTML/ },
		{ name: 'too big', make: () => response(html(9110, 'x'.repeat(10 * 1024 * 1024))), error: /byte limit/ },
		{ name: 'redirect', make: () => response('', { status: 302 }), error: /HTTP 302/ },
		{ name: 'partial', make: () => response(html(), { status: 206 }), error: /HTTP 206/ },
	];
	for (const entry of cases) {
		await t.test(entry.name, async () => {
			await assert.rejects(refresh(['9110'], dir, async () => entry.make()), entry.error);
			assert.deepEqual(await readdir(dir), ['manifest.json']);
		});
	}
	await assert.rejects(
		refresh(['9110'], dir, async () => {
			throw new Error('network timeout');
		}),
		/network timeout/,
	);
});

test('offline check rejects corruption; fetch does not silently bless it', async (t) => {
	const dir = await corpus(t);
	await refresh(['9110'], dir, async () => response());
	await writeFile(join(dir, 'rfc9110.html'), html(9110, 'tampered'));
	await assert.rejects(check(dir), /differs from manifest/);
	await assert.rejects(refresh(['9110'], dir, async () => assert.fail('must not fetch')), /differs from manifest/);
});

test('offline check rejects missing and untracked HTML', async (t) => {
	const dir = await corpus(t);
	await writeFile(join(dir, 'unexpected.html'), html());
	await assert.rejects(check(dir), /missing from manifest/);
	await rm(join(dir, 'unexpected.html'));
	await refresh(['9110'], dir, async () => response());
	await rm(join(dir, 'rfc9110.html'));
	await assert.rejects(check(dir), /ENOENT/);
});

test('manifest rejects duplicate entries and untrusted paths/URLs', async (t) => {
	const dir = await corpus(t);
	await refresh(['9110'], dir, async () => response());
	const original = await check(dir);
	for (
		const entry of [
			{ ...original.documents[0], file: '../outside.html' },
			{ ...original.documents[0], url: 'https://example.com/9110.html' },
			{ ...original.documents[0], sha256: null },
			{ ...original.documents[0], bytes: 0 },
		]
	) {
		await writeFile(join(dir, 'manifest.json'), JSON.stringify({ version: 1, documents: [entry] }));
		await assert.rejects(check(dir), /Invalid or duplicate/);
	}
	await writeFile(
		join(dir, 'manifest.json'),
		JSON.stringify({ ...original, documents: [...original.documents, ...original.documents] }),
	);
	await assert.rejects(check(dir), /Invalid or duplicate/);
});

test('refresh adds documents in numeric order, preserving existing unselected documents', async (t) => {
	const dir = await corpus(t);
	await refresh(['9110'], dir, async () => response());
	await refresh(
		['9530', '9111'],
		dir,
		async (url) => response(html(Number(/rfc([0-9]+)\.html$/.exec(String(url))?.[1]))),
	);
	assert.deepEqual((await check(dir)).documents.map((entry) => entry.number), [9110, 9111, 9530]);
	assert.equal(await readFile(join(dir, 'rfc9110.html'), 'utf8'), html());
});
