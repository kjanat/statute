import { execFileSync } from 'node:child_process';
import { lstatSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkConsistency } from './check-lol-html.mjs';

export function eligible(pr, repository) {
	return pr?.state === 'open' && pr.user?.login === 'renovate[bot]'
		&& pr.head?.repo?.full_name === repository && pr.base?.repo?.full_name === repository
		&& pr.head.ref === 'renovate/lol-html-source-refresh';
}

export function allowedPath(path) {
	return typeof path === 'string'
		&& /^research\/htmlrewrite\/guest\/(?:Cargo\.(?:toml|lock)|vendor\/README\.md|vendor\/lol_html\/[\w./-]+)$/.test(
			path,
		)
		&& !path.split('/').some(part => ['', '.', '..', '.git'].includes(part));
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
		if (!allowedPath(path)) throw new Error(`Unexpected refresh path: ${path}`);
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

export function createRequest(root, pr, repository) {
	if (!eligible(pr, repository)) throw new Error('Only same-repository Renovate LOL HTML PRs can be refreshed');
	const version = checkConsistency(join(root, 'research/htmlrewrite/guest'));
	return { repository, pull_request: pr.number, head_sha: pr.head.sha, version, changes: collectChanges(root) };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	try {
		const output = process.argv[2];
		if (!output) throw new Error('Usage: node renovate-lol-html.mjs OUTPUT_JSON');
		const { pull_request: pr } = JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, 'utf8'));
		const request = createRequest(process.env.GITHUB_WORKSPACE, pr, process.env.GITHUB_REPOSITORY);
		mkdirSync(dirname(output), { recursive: true });
		writeFileSync(output, JSON.stringify(request));
	} catch (error) {
		console.error(error.message);
		process.exitCode = 1;
	}
}
