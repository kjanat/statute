import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { targetVersion } from './check-lol-html.mjs';
import publish, { readArtifact, validateRequest } from './publish-lol-html.mjs';

const repository = 'kjanat/statute';
const guest = join(dirname(fileURLToPath(import.meta.url)), 'guest');
const head = 'a'.repeat(40);
const sourcePath = 'research/htmlrewrite/guest/vendor/lol_html/new.rs';
const addition = (path, text) => ({ path, contents: Buffer.from(text).toString('base64') });

function zip(request, name = 'request.json', duplicates = false, size = 0, symlink = false) {
	return execFileSync('python3', [
		'-c',
		`
import io, sys, zipfile
output = io.BytesIO()
data = sys.stdin.buffer.read()
with zipfile.ZipFile(output, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
    entry = zipfile.ZipInfo(sys.argv[1])
    entry.compress_type = zipfile.ZIP_DEFLATED
    if sys.argv[4] == 'true': entry.external_attr = 0o120777 << 16
    archive.writestr(entry, data if not int(sys.argv[3]) else b'x' * int(sys.argv[3]))
    if sys.argv[2] == 'true': archive.writestr(sys.argv[1], data)
sys.stdout.buffer.write(output.getvalue())
`,
		name,
		String(duplicates),
		String(size),
		String(symlink),
	], { input: JSON.stringify(request), stdio: 'pipe', maxBuffer: 10 << 20 });
}

function fixture() {
	const pr = {
		number: 293,
		state: 'open',
		user: { login: 'renovate[bot]' },
		head: { ref: 'renovate/lol-html-source-refresh', sha: head, repo: { full_name: repository } },
		base: { ref: 'master', repo: { full_name: repository } },
	};
	const run = {
		id: 42,
		run_attempt: 1,
		head_sha: head,
		event: 'pull_request',
		conclusion: 'success',
		path: '.github/workflows/renovate-lol-html.yml',
		head_branch: pr.head.ref,
		repository: { full_name: repository },
		head_repository: { full_name: repository },
		pull_requests: [{ number: 293 }],
	};
	const request = {
		repository,
		pull_request: 293,
		head_sha: head,
		version: targetVersion(),
		changes: {
			additions: [addition(sourcePath, 'new source')],
			deletions: [{ path: sourcePath.replace('new.rs', 'old.rs') }],
		},
	};
	const f = {
		pr,
		run,
		request,
		commits: [],
		reads: [],
		artifacts: [{ id: 12, name: 'lol-html-refresh-1', expired: false, size_in_bytes: 100 }],
		context: {
			repo: { owner: 'kjanat', repo: 'statute' },
			payload: { workflow_run: { id: 42, run_attempt: 1 }, repository: { default_branch: 'master' } },
		},
		core: { info() {} },
	};
	f.github = {
		paginate: async () => f.artifacts,
		rest: {
			actions: {
				getWorkflowRun: async () => ({ data: f.run }),
				listWorkflowRunArtifacts() {},
				downloadArtifact: async () => ({ data: zip(f.request) }),
				createWorkflowDispatch: async () => {
					throw new Error('Privileged workflow dispatch is forbidden');
				},
			},
			pulls: { get: async () => ({ data: f.pr }) },
			repos: {
				getContent: async ({ path, ref }) => {
					f.reads.push({ path, ref });
					const content = readFileSync(join(guest, path.slice('research/htmlrewrite/guest/'.length)));
					return {
						data: { type: 'file', encoding: 'base64', content: content.toString('base64'), size: content.length },
					};
				},
			},
		},
		graphql: async (query, { input }) => {
			f.commits.push(input);
			return { createCommitOnBranch: { commit: { oid: 'b'.repeat(40) } } };
		},
	};
	return f;
}

test('publisher preserves source changes, leases the head and never dispatches PR workflows', async () => {
	const f = fixture();
	await publish(f);
	assert.equal(f.commits.length, 1);
	assert.equal(f.commits[0].expectedHeadOid, head);
	assert.deepEqual(f.commits[0].fileChanges, f.request.changes);
	assert.ok(f.reads.every(read => read.ref === head));
});

test('artifact code remains inert data in the privileged publisher', async t => {
	const f = fixture();
	const root = mkdtempSync(join(tmpdir(), 'statute-publisher-attack-'));
	t.after(() => rmSync(root, { recursive: true, force: true }));
	const marker = join(root, 'executed');
	const code = `require('node:fs').writeFileSync(${JSON.stringify(marker)}, 'compromised');`;
	f.request.changes.additions = [addition(sourcePath.replace('new.rs', 'evil.mjs'), code)];
	await publish(f);
	assert.equal(existsSync(marker), false);
	assert.equal(Buffer.from(f.commits[0].fileChanges.additions[0].contents, 'base64').toString(), code);
});

test('ZIP paths, duplicate members, symlinks and oversized content are rejected without extraction', () => {
	assert.throws(() => readArtifact(zip({}, '../request.json')), /Expected only request.json/);
	assert.throws(() => readArtifact(zip({}, 'request.json', true)), /Expected only request.json/);
	assert.throws(() => readArtifact(zip({}, 'request.json', false, 0, true)), /Invalid refresh artifact member/);
	assert.throws(
		() => readArtifact(zip({}, 'request.json', false, (8 << 20) + 1)),
		/exceeds 8 MiB|Invalid refresh artifact member/,
	);
});

test('wrong run event, repository, workflow, attempt, status and branch fail closed', async () => {
	for (
		const mutate of [
			f => f.run.event = 'push',
			f => f.run.actor = { login: 'github-actions[bot]' },
			f => f.run.head_repository.full_name = 'outsider/statute',
			f => f.run.repository.full_name = 'outsider/statute',
			f => f.run.path = '.github/workflows/ci.yml',
			f => f.run.run_attempt = 2,
			f => f.run.conclusion = 'failure',
			f => f.run.head_branch = 'another-branch',
			f => f.run.pull_requests = [],
		]
	) {
		const f = fixture();
		mutate(f);
		await assert.rejects(publish(f), /Ineligible/);
		assert.deepEqual(f.commits, []);
	}
});

test('foreign, closed and stale PRs fail before publication', async () => {
	for (
		const mutate of [
			f => f.pr.head.repo.full_name = 'outsider/statute',
			f => f.pr.state = 'closed',
			f => f.pr.head.sha = 'c'.repeat(40),
			f => f.pr.base.ref = 'other-base',
		]
	) {
		const f = fixture();
		mutate(f);
		await assert.rejects(publish(f), /PR changed or is ineligible/);
		assert.deepEqual(f.commits, []);
	}
});

test('missing, ambiguous, expired and oversized artifacts fail closed', async () => {
	for (
		const mutate of [
			f => f.artifacts = [],
			f => f.artifacts.push({ ...f.artifacts[0] }),
			f => f.artifacts[0].expired = true,
			f => f.artifacts[0].size_in_bytes = (8 << 20) + 1,
		]
	) {
		const f = fixture();
		mutate(f);
		await assert.rejects(publish(f), /refresh artifact/);
		assert.deepEqual(f.commits, []);
	}
});

test('request identity, paths, duplicates, metadata deletions and encoding are validated independently', () => {
	for (
		const mutate of [
			f => f.request.head_sha = 'c'.repeat(40),
			f => f.request.pull_request = 294,
			f => f.request.repository = 'outsider/statute',
			f => f.request.version = 'bad',
			f => f.request.changes.additions[0].path = '.github/workflows/ci.yml',
			f => f.request.changes.additions[0].path = sourcePath.replace('new.rs', '../escape'),
			f => f.request.changes.additions[0].path = sourcePath.replace('new.rs', '.git/config'),
			f => f.request.changes.additions.push({ ...f.request.changes.additions[0] }),
			f => f.request.changes.deletions = [{ path: 'research/htmlrewrite/guest/Cargo.toml' }],
			f => f.request.changes.additions[0].contents = '!invalid!',
		]
	) {
		const f = fixture();
		mutate(f);
		assert.throws(() => validateRequest(f.request, f.run, f.pr, repository));
	}
});

test('version or checksum drift in valid artifact data prevents a commit', async () => {
	const f = fixture();
	f.request.version = '99.0.0';
	await assert.rejects(publish(f), /Refresh version does not match/);
	assert.deepEqual(f.commits, []);
	f.request.version = targetVersion();
	f.request.changes.additions.push(
		addition(
			'research/htmlrewrite/guest/vendor/README.md',
			readFileSync(join(guest, 'vendor/README.md'), 'utf8').replace(/`[a-f0-9]{64}`/, `\`${'0'.repeat(64)}\``),
		),
	);
	await assert.rejects(publish(f), /checksum differs/);
	assert.deepEqual(f.commits, []);
});

test('a concurrent head change is rejected and a no-op makes no commit', async () => {
	const f = fixture();
	let reads = 0;
	f.github.rest.pulls.get = async () => ({
		data: ++reads === 1 ? f.pr : { ...f.pr, head: { ...f.pr.head, sha: 'c'.repeat(40) } },
	});
	await assert.rejects(publish(f), /PR changed during artifact validation/);
	assert.deepEqual(f.commits, []);
	const noop = fixture();
	noop.request.changes = { additions: [], deletions: [] };
	await publish(noop);
	assert.deepEqual(noop.commits, []);
});
