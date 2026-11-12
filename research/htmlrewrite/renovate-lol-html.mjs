import { execFileSync } from 'node:child_process';
import { lstatSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { checkConsistency } from './check-lol-html.mjs';

export function eligible(pr, repository) {
	return pr?.state === 'open' && pr.user?.login === 'renovate[bot]'
		&& pr.head?.repo?.full_name === repository && pr.base?.repo?.full_name === repository
		&& pr.head.ref === 'renovate/lol-html-source-refresh';
}

export function collectChanges(root) {
	const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).split('\0').filter(Boolean);
	const paths = [
		...new Set([
			...git(['diff', '--no-renames', '--name-only', '-z', 'HEAD', '--']),
			...git(['ls-files', '--others', '--exclude-standard', '-z']),
		]),
	];
	return paths.reduce((changes, path) => {
		if (
			!/^research\/htmlrewrite\/guest\/(?:Cargo\.(?:toml|lock)|vendor\/README\.md|vendor\/lol_html\/[\w./-]+)$/.test(
				path,
			)
			|| path.split('/').includes('..')
		) throw new Error(`Unexpected refresh path: ${path}`);
		const absolute = join(root, path);
		try {
			if (!lstatSync(absolute).isFile()) throw new Error(`Refresh path is not a regular file: ${path}`);
			changes.additions.push({ path, contents: readFileSync(absolute).toString('base64') });
		} catch (error) {
			if (error.code !== 'ENOENT') throw error;
			changes.deletions.push({ path });
		}
		return changes;
	}, { additions: [], deletions: [] });
}

export default async function publish({ github, context, core, root = process.env.GITHUB_WORKSPACE }) {
	const pr = context.payload.pull_request;
	const repository = `${context.repo.owner}/${context.repo.repo}`;
	if (!eligible(pr, repository)) throw new Error('Only same-repository Renovate LOL HTML PRs can be refreshed');
	const version = checkConsistency(join(root, 'research/htmlrewrite/guest'));
	const changes = collectChanges(root);
	if (changes.additions.length + changes.deletions.length === 0) {
		core.info(`LOL HTML ${version} is already refreshed`);
		return;
	}
	const { data: current } = await github.rest.pulls.get({ ...context.repo, pull_number: pr.number });
	if (!eligible(current, repository) || current.head.sha !== pr.head.sha) {
		throw new Error('PR changed during source refresh; rerun on its latest head');
	}
	const result = await github.graphql(
		`mutation($input: CreateCommitOnBranchInput!) {
		createCommitOnBranch(input: $input) { commit { oid } }
	}`,
		{
			input: {
				branch: { repositoryNameWithOwner: repository, branchName: pr.head.ref },
				expectedHeadOid: pr.head.sha,
				message: { headline: `chore(deps): refresh patched LOL HTML ${version} source` },
				fileChanges: changes,
			},
		},
	);
	core.info(`Published LOL HTML ${version}: ${result.createCommitOnBranch.commit.oid}`);
	// GITHUB_TOKEN commits need explicit dispatches for unattended CI.
	const workflows = [
		'ci.yml',
		'codeql.yml',
		'research-htmlrewrite.yml',
		'research-htmlrewrite-binaryen.yml',
		'comment-cop.yml',
	];
	const results = await Promise.allSettled(workflows.map(workflow_id =>
		github.rest.actions.createWorkflowDispatch({
			...context.repo,
			workflow_id,
			ref: pr.head.ref,
			...(workflow_id === 'comment-cop.yml' ? { inputs: { pull_request: String(pr.number) } } : {}),
		})
	));
	const failed = results.flatMap((result, i) => result.status === 'rejected' ? [workflows[i]] : []);
	if (failed.length) {
		throw new Error(
			`Refresh committed, but CI dispatch failed for: ${failed.join(', ')}. Dispatch these workflows on ${pr.head.ref}`,
		);
	}
}
