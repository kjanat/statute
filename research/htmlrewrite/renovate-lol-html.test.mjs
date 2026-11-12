import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import publish, { collectChanges, eligible } from './renovate-lol-html.mjs';

const repository = 'kjanat/statute';
const original = join(dirname(fileURLToPath(import.meta.url)), 'guest');
const pr = {
	number: 293,
	state: 'open',
	user: { login: 'renovate[bot]' },
	head: { ref: 'renovate/lol-html-source-refresh', sha: 'a'.repeat(40), repo: { full_name: repository } },
	base: { repo: { full_name: repository } },
};

function fixture(t) {
	const root = mkdtempSync(join(tmpdir(), 'statute-lol-html-publish-'));
	t.after(() => rmSync(root, { recursive: true, force: true }));
	const guest = join(root, 'research/htmlrewrite/guest');
	for (const path of ['Cargo.toml', 'Cargo.lock', 'vendor/README.md', 'vendor/lol_html/Cargo.toml']) {
		mkdirSync(dirname(join(guest, path)), { recursive: true });
		cpSync(join(original, path), join(guest, path));
	}
	const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: 'pipe' });
	git(['init', '--quiet']);
	git(['add', '.']);
	git(['-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '--quiet', '-m', 'baseline']);
	const context = { repo: { owner: 'kjanat', repo: 'statute' }, payload: { pull_request: structuredClone(pr) } };
	const calls = [];
	const github = {
		rest: {
			pulls: { get: async () => ({ data: context.payload.pull_request }) },
			actions: {
				createWorkflowDispatch: async params => {
					calls.push(params);
				},
			},
		},
		graphql: async (query, params) => {
			calls.push(params);
			return { createCommitOnBranch: { commit: { oid: 'b'.repeat(40) } } };
		},
	};
	return { root, guest, context, github, calls, core: { info() {} } };
}

test('only the same-repository Renovate source-refresh PR is eligible', () => {
	assert.ok(eligible(pr, repository));
	for (
		const mutate of [
			p => p.head.repo.full_name = 'outsider/statute',
			p => p.base.repo.full_name = 'outsider/statute',
			p => p.user.login = 'outsider',
			p => p.head.ref = 'renovate/another-package',
			p => p.state = 'closed',
		]
	) {
		const candidate = structuredClone(pr);
		mutate(candidate);
		assert.equal(eligible(candidate, repository), false);
	}
});

test('publication includes new and deleted source files, leases the head, and dispatches fresh CI', async t => {
	const f = fixture(t);
	const manifest = join(f.guest, 'vendor/lol_html/Cargo.toml');
	writeFileSync(manifest, `${readFileSync(manifest, 'utf8')}\n`);
	writeFileSync(join(f.guest, 'vendor/lol_html/new.rs'), 'new source\n');
	await publish(f);
	const input = f.calls[0].input;
	assert.equal(input.expectedHeadOid, pr.head.sha);
	assert.equal(input.branch.branchName, pr.head.ref);
	assert.equal(input.fileChanges.additions.length, 2);
	assert.ok(
		input.fileChanges.additions.some(file => Buffer.from(file.contents, 'base64').toString() === 'new source\n'),
	);
	assert.deepEqual(f.calls.slice(1).map(call => call.workflow_id).sort(), [
		'ci.yml',
		'codeql.yml',
		'comment-cop.yml',
		'research-htmlrewrite-binaryen.yml',
		'research-htmlrewrite.yml',
	]);
	assert.ok(f.calls.slice(1).every(call => call.ref === pr.head.ref));
	assert.deepEqual(f.calls.find(call => call.workflow_id === 'comment-cop.yml').inputs, { pull_request: '293' });
	rmSync(manifest);
	assert.deepEqual(collectChanges(f.root).deletions, [{
		path: 'research/htmlrewrite/guest/vendor/lol_html/Cargo.toml',
	}]);
});

test('no-op refresh makes no commit or dispatch', async t => {
	const f = fixture(t);
	await publish(f);
	assert.deepEqual(f.calls, []);
});

test('unrelated changes and symlinks cannot enter the generated commit', t => {
	const f = fixture(t);
	const unexpected = join(f.root, 'workflow.yml');
	writeFileSync(unexpected, 'unexpected');
	assert.throws(() => collectChanges(f.root), /Unexpected refresh path/);
	rmSync(unexpected);
	symlinkSync('/etc/passwd', join(f.guest, 'vendor/lol_html/escape'));
	assert.throws(() => collectChanges(f.root), /not a regular file/);
});

test('a stale head or failed commit never dispatches CI', async t => {
	const f = fixture(t);
	writeFileSync(join(f.guest, 'vendor/lol_html/new.rs'), 'source');
	f.github.rest.pulls.get = async () => ({ data: { ...pr, head: { ...pr.head, sha: 'c'.repeat(40) } } });
	await assert.rejects(publish(f), /PR changed/);
	assert.deepEqual(f.calls, []);
	f.github.rest.pulls.get = async () => ({ data: pr });
	f.github.graphql = async () => {
		throw new Error('expectedHeadOid mismatch');
	};
	await assert.rejects(publish(f), /expectedHeadOid mismatch/);
	assert.deepEqual(f.calls, []);
});

test('all CI dispatches are attempted and partial failure is reported after publication', async t => {
	const f = fixture(t);
	writeFileSync(join(f.guest, 'vendor/lol_html/new.rs'), 'source');
	f.github.rest.actions.createWorkflowDispatch = async params => {
		f.calls.push(params);
		if (params.workflow_id === 'ci.yml') throw new Error('unavailable');
	};
	await assert.rejects(publish(f), /Refresh committed, but CI dispatch failed for: ci\.yml/);
	assert.equal(f.calls.length, 6);
});
