import { execFileSync } from 'node:child_process';
import { checkFiles } from './check-lol-html.mjs';
import { allowedPath, eligible } from './renovate-lol-html.mjs';

const MAX_BYTES = 8 << 20;
const PREFIX = 'research/htmlrewrite/guest/';
const METADATA = ['Cargo.toml', 'Cargo.lock', 'vendor/README.md', 'vendor/lol_html/Cargo.toml'];

export function readArtifact(archive) {
	if (archive.length > MAX_BYTES) throw new Error('Refresh artifact exceeds 8 MiB');
	// Read one bounded ZIP member directly; no artifact paths reach the filesystem.
	const json = execFileSync('python3', [
		'-c',
		`
import io, stat, sys, zipfile
with zipfile.ZipFile(io.BytesIO(sys.stdin.buffer.read())) as archive:
    entries = archive.infolist()
    if len(entries) != 1 or entries[0].filename != 'request.json':
        raise ValueError('Expected only request.json in refresh artifact')
    entry = entries[0]
    if entry.file_size > ${MAX_BYTES} or stat.S_IFMT(entry.external_attr >> 16) not in (0, stat.S_IFREG):
        raise ValueError('Invalid refresh artifact member')
    sys.stdout.buffer.write(archive.read(entry))
`,
	], { input: archive, encoding: 'utf8', timeout: 10000, maxBuffer: MAX_BYTES, stdio: 'pipe' });
	return JSON.parse(json);
}

export function validateRequest(request, run, pr, repository) {
	if (
		request?.repository !== repository || request.pull_request !== pr.number || request.head_sha !== run.head_sha
		|| typeof request.version !== 'string' || !/^\d+\.\d+\.\d+$/.test(request.version)
	) throw new Error('Refresh request does not match the workflow run');
	const changes = request.changes;
	if (
		!Array.isArray(changes?.additions) || !Array.isArray(changes?.deletions)
		|| changes.additions.length + changes.deletions.length > 1024
	) throw new Error('Invalid refresh changes');
	const paths = new Set();
	let bytes = 0;
	for (const [kind, files] of Object.entries({ additions: changes.additions, deletions: changes.deletions })) {
		for (const file of files) {
			if (!allowedPath(file?.path) || paths.has(file.path)) throw new Error('Invalid or duplicate refresh path');
			paths.add(file.path);
			if (kind === 'additions') {
				if (
					typeof file.contents !== 'string' || Buffer.from(file.contents, 'base64').toString('base64') !== file.contents
				) {
					throw new Error('Invalid refresh file encoding');
				}
				bytes += Buffer.byteLength(file.contents, 'base64');
			} else if (METADATA.includes(file.path.slice(PREFIX.length))) {
				throw new Error('Refresh cannot delete required metadata');
			}
		}
	}
	if (bytes > MAX_BYTES) throw new Error('Refresh changes exceed 8 MiB');
	return {
		additions: changes.additions.map(({ path, contents }) => ({ path, contents })),
		deletions: changes.deletions.map(({ path }) => ({ path })),
	};
}

export default async function publish({ github, context, core }) {
	const repository = `${context.repo.owner}/${context.repo.repo}`;
	const event = context.payload.workflow_run;
	const { data: run } = await github.rest.actions.getWorkflowRun({ ...context.repo, run_id: event.id });
	if (
		run.event !== 'pull_request' || run.conclusion !== 'success' || run.actor?.login === 'github-actions[bot]'
		|| run.path !== '.github/workflows/renovate-lol-html.yml'
		|| run.repository?.full_name !== repository || run.head_repository?.full_name !== repository
		|| run.head_branch !== 'renovate/lol-html-source-refresh' || !Number.isSafeInteger(run.run_attempt)
		|| run.run_attempt < 1 || run.run_attempt !== event.run_attempt || run.pull_requests?.length !== 1
	) {
		throw new Error('Ineligible LOL HTML generation run');
	}
	const { data: pr } = await github.rest.pulls.get({ ...context.repo, pull_number: run.pull_requests[0].number });
	if (
		!eligible(pr, repository) || pr.head.sha !== run.head_sha
		|| pr.base.ref !== context.payload.repository.default_branch
	) throw new Error('PR changed or is ineligible for publication');
	const artifacts = await github.paginate(github.rest.actions.listWorkflowRunArtifacts, {
		...context.repo,
		run_id: run.id,
	});
	const matches = artifacts.filter(artifact => artifact.name === `lol-html-refresh-${run.run_attempt}`);
	if (matches.length !== 1 || matches[0].expired || matches[0].size_in_bytes > MAX_BYTES) {
		throw new Error('Missing, expired, ambiguous or oversized refresh artifact');
	}
	const { data: archive } = await github.rest.actions.downloadArtifact({
		...context.repo,
		artifact_id: matches[0].id,
		archive_format: 'zip',
	});
	const request = readArtifact(Buffer.from(archive));
	const changes = validateRequest(request, run, pr, repository);
	if (changes.additions.length + changes.deletions.length === 0) {
		core.info('LOL HTML source is already refreshed');
		return;
	}
	const files = new Map(
		changes.additions.map(file => [file.path, Buffer.from(file.contents, 'base64').toString('utf8')]),
	);
	for (const path of METADATA) {
		if (files.has(PREFIX + path)) continue;
		const { data: file } = await github.rest.repos.getContent({
			...context.repo,
			path: PREFIX + path,
			ref: run.head_sha,
		});
		if (file.type !== 'file' || file.encoding !== 'base64' || file.size > 1 << 20) {
			throw new Error('Invalid PR metadata file');
		}
		files.set(PREFIX + path, Buffer.from(file.content, 'base64').toString('utf8'));
	}
	if (checkFiles(path => files.get(PREFIX + path)) !== request.version) {
		throw new Error('Refresh version does not match its files');
	}
	const { data: current } = await github.rest.pulls.get({ ...context.repo, pull_number: pr.number });
	if (!eligible(current, repository) || current.head.sha !== run.head_sha || current.base.ref !== pr.base.ref) {
		throw new Error('PR changed during artifact validation');
	}
	const result = await github.graphql(
		`mutation($input: CreateCommitOnBranchInput!) {
		createCommitOnBranch(input: $input) { commit { oid } }
	}`,
		{
			input: {
				branch: { repositoryNameWithOwner: repository, branchName: pr.head.ref },
				expectedHeadOid: run.head_sha,
				message: { headline: `chore(deps): refresh patched LOL HTML ${request.version} source` },
				fileChanges: changes,
			},
		},
	);
	core.info(
		`Published LOL HTML ${request.version}: ${result.createCommitOnBranch.commit.oid}. Approve the generated commit's PR workflows in GitHub to run CI.`,
	);
}
