// @ts-check
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { goTags, syncImages } from './sync-go-images.mjs';

const digest = `sha256:${'a'.repeat(64)}`;
const otherDigest = `sha256:${'b'.repeat(64)}`;

test('go.mod selects stable minor and exact toolchain tags for both variants', () => {
	assert.deepEqual(goTags('module example\n\ngo 1.27\n\ntoolchain go1.27.3\n'), [
		'1.27-bookworm',
		'1.27-alpine',
		'1.27.3-bookworm',
		'1.27.3-alpine',
	]);
	assert.deepEqual(goTags('go 1.28.0\r\ntoolchain go1.28.1\r\n'), [
		'1.28-bookworm',
		'1.28-alpine',
		'1.28.1-bookworm',
		'1.28.1-alpine',
	]);
	for (
		const mod of [
			'go 1.27',
			'go 1.27\ntoolchain go1.28.1',
			'go 1.27\ntoolchain go1.27rc1',
			'go 1.27\ntoolchain default',
		]
	) {
		assert.throws(() => goTags(mod), /matching stable/);
	}
});

test('copies captured digests with every architecture and verifies each destination', () => {
	/** @type {string[][]} */
	const calls = [];
	/** @type {string[]} */
	const reports = [];
	const tags = ['1.27-bookworm', '1.27-alpine'];
	syncImages(tags, (args) => {
		calls.push(args);
		return args.at(-1)?.endsWith('alpine') ? otherDigest : digest;
	}, (line) => reports.push(line));
	assert.deepEqual(calls.map((args) => args[0]), ['inspect', 'inspect', 'copy', 'inspect', 'copy', 'inspect']);
	for (const [index, tag] of tags.entries()) {
		const expected = index === 0 ? digest : otherDigest;
		assert.deepEqual(calls[2 + index * 2], [
			'copy',
			'--authfile',
			'/auth/auth.json',
			'--all',
			'--preserve-digests',
			'--retry-times',
			'3',
			`docker://public.ecr.aws/docker/library/golang@${expected}`,
			`docker://ghcr.io/kjanat/golang:${tag}`,
		]);
		assert.equal(calls[3 + index * 2].at(-1), `docker://ghcr.io/kjanat/golang:${tag}`);
		assert.equal(reports[index], `FROM ghcr.io/kjanat/golang:${tag}@${expected}`);
	}
});

test('source validation completes before any tag is written', () => {
	for (const failure of ['invalid', 'registry error']) {
		let copies = 0;
		assert.throws(() =>
			syncImages(['1.27-bookworm', '1.27-alpine'], (args) => {
				if (args[0] === 'copy') copies++;
				if (args.at(-1)?.endsWith('alpine')) {
					if (failure === 'registry error') throw new Error(failure);
					return 'sha256:wrong';
				}
				return digest;
			}, () => assert.fail('failed source was reported as synchronized')), /invalid digest|registry error/);
		assert.equal(copies, 0);
	}
	for (const tags of [[], ['latest'], ['1.27-alpine;echo bad']]) {
		assert.throws(() => syncImages(tags, () => assert.fail('invalid tag reached Skopeo'), () => {}), /stable Go/);
	}
});

test('copy and verification errors stop later writes and do not report success', () => {
	for (const failure of ['copy', 'mismatch', 'inspect']) {
		let copies = 0;
		assert.throws(() =>
			syncImages(['1.27-bookworm', '1.27-alpine'], (args) => {
				if (args[0] === 'copy') {
					copies++;
					if (failure === 'copy') throw new Error('copy rejected');
				}
				if (args[0] === 'inspect' && args.at(-1)?.includes('ghcr.io')) {
					if (failure === 'inspect') throw new Error('registry unavailable');
					return otherDigest;
				}
				return digest;
			}, () => assert.fail('unverified image was reported')), /copy rejected|digest differs|registry unavailable/);
		assert.equal(copies, 1);
	}
});

for (const failure of ['', 'login', 'copy']) {
	test(`CLI passes credentials through stdin and removes auth files after ${failure || 'success'}`, (t) => {
		const dir = mkdtempSync(join(tmpdir(), 'statute-image-sync-test-'));
		t.after(() => rmSync(dir, { recursive: true, force: true }));
		const log = join(dir, 'calls.jsonl');
		writeFileSync(
			join(dir, 'docker'),
			`#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
const auth = args[args.indexOf('--volume') + 1].slice(0, -6);
const command = args[args.indexOf('--volume') + 3];
const input = fs.readFileSync(0, 'utf8');
fs.appendFileSync(process.env.TEST_LOG, JSON.stringify({ args, auth, command,
  tokenInEnv: Boolean(process.env.GH_TOKEN), loginInput: input === 'fixture-token\\n' }) + '\\n');
if (command === 'login') fs.writeFileSync(auth + '/auth.json', '{}');
if (command === process.env.TEST_FAIL) { console.error('failed fixture-token'); process.exit(1); }
if (command === 'inspect') console.log('${digest}');
`,
			{ mode: 0o700 },
		);
		const summary = join(dir, 'summary.md');
		const result = spawnSync(process.execPath, ['scripts/sync-go-images.mjs'], {
			cwd: new URL('..', import.meta.url),
			encoding: 'utf8',
			env: {
				...process.env,
				PATH: `${dir}:${process.env.PATH}`,
				GH_TOKEN: 'fixture-token',
				GH_ACTOR: 'fixture-user',
				SKOPEO_IMAGE: `quay.io/skopeo/stable@${digest}`,
				GITHUB_STEP_SUMMARY: summary,
				TEST_LOG: log,
				TEST_FAIL: failure,
			},
		});
		assert.equal(result.status, failure ? 1 : 0, result.stderr);
		assert.ok(!result.stdout.includes('fixture-token'));
		assert.ok(!result.stderr.includes('fixture-token'));
		/** @type {{args: string[], auth: string, command: string, tokenInEnv: boolean, loginInput: boolean}[]} */
		const calls = readFileSync(log, 'utf8').trim().split('\n').map((line) => JSON.parse(line));
		assert.equal(calls[0].command, 'login');
		assert.ok(calls[0].args.includes('--password-stdin'));
		assert.ok(calls[0].loginInput);
		for (const call of calls) {
			assert.equal(call.tokenInEnv, false);
			assert.ok(!call.args.some((arg) => arg.includes('fixture-token')));
			assert.equal(existsSync(call.auth), false);
		}
		if (!failure) assert.equal(readFileSync(summary, 'utf8').match(/FROM ghcr.io/g)?.length, 4);
		else assert.equal(existsSync(summary), false);
	});
}
