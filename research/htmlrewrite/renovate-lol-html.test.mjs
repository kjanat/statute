import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { collectChanges, createRequest, eligible } from './renovate-lol-html.mjs';

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
	const root = mkdtempSync(join(tmpdir(), 'statute-lol-html-generate-'));
	t.after(() => rmSync(root, { recursive: true, force: true }));
	const guest = join(root, 'research/htmlrewrite/guest');
	for (const path of ['Cargo.toml', 'Cargo.lock', 'vendor/README.md', 'vendor/lol_html/Cargo.toml']) {
		mkdirSync(dirname(join(guest, path)), { recursive: true });
		cpSync(join(original, path), join(guest, path));
	}
	writeFileSync(join(guest, 'vendor/lol_html/old.rs'), 'old source\n');
	const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: 'pipe' });
	git(['init', '--quiet']);
	git(['add', '.']);
	git(['-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '--quiet', '-m', 'baseline']);
	return { root, guest };
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

test('generation serializes additions and deletions with the inspected PR identity', t => {
	const f = fixture(t);
	writeFileSync(join(f.guest, 'vendor/lol_html/new.rs'), 'new source\n');
	rmSync(join(f.guest, 'vendor/lol_html/old.rs'));
	const request = createRequest(f.root, pr, repository);
	assert.equal(request.head_sha, pr.head.sha);
	assert.equal(request.pull_request, 293);
	assert.equal(request.repository, repository);
	assert.deepEqual(request.changes.deletions, [{ path: 'research/htmlrewrite/guest/vendor/lol_html/old.rs' }]);
	assert.equal(Buffer.from(request.changes.additions[0].contents, 'base64').toString(), 'new source\n');
});

test('no-op refresh serializes an empty change set', t => {
	assert.deepEqual(createRequest(fixture(t).root, pr, repository).changes, { additions: [], deletions: [] });
});

test('unrelated changes and symlinks cannot enter the generation request', t => {
	const f = fixture(t);
	const unexpected = join(f.root, 'workflow.yml');
	writeFileSync(unexpected, 'unexpected');
	assert.throws(() => collectChanges(f.root), /Unexpected refresh path/);
	rmSync(unexpected);
	symlinkSync('/etc/passwd', join(f.guest, 'vendor/lol_html/escape'));
	assert.throws(() => collectChanges(f.root), /not a regular file/);
});

test('CLI writes the artifact outside the checkout without a GitHub credential', t => {
	const f = fixture(t);
	const event = join(f.root, '.git/event.json');
	const output = join(f.root, '.git/artifact/request.json');
	writeFileSync(event, JSON.stringify({ pull_request: pr }));
	execFileSync(process.execPath, [fileURLToPath(new URL('./renovate-lol-html.mjs', import.meta.url)), output], {
		env: {
			...process.env,
			GITHUB_EVENT_PATH: event,
			GITHUB_WORKSPACE: f.root,
			GITHUB_REPOSITORY: repository,
			GITHUB_TOKEN: '',
			GH_TOKEN: '',
		},
	});
	assert.equal(JSON.parse(readFileSync(output)).head_sha, pr.head.sha);
});
