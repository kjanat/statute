#!/usr/bin/env node
// @ts-check
// Comment Cop: flags paragraph-length implementation comments added to Go
// source in a pull request, and resolves its own stale review threads.
//
// Two entry points, one implementation:
//
//   - GitHub Actions: `.github/workflows/comment-cop.yml` imports this file
//     and calls the default export with the `actions/github-script` objects.
//   - Locally: `node scripts/comment-cop.mjs kjanat/statute 72`
//     Requires Node >= 24.2 (for `import.meta.main`) or Bun, and a
//     `GITHUB_TOKEN` in the environment.
//
// Local runs are DRY RUN unless `--apply` is passed: they scan, print every
// finding and every thread that would be resolved, and call no mutating API.

import { createHash } from 'node:crypto';

/* ------------------------------------------------------------------- types */

/**
 * One flagged comment block, located in the head-side file.
 * @typedef {object} Group
 * @property {string} path
 * @property {number} start
 * @property {number} end
 * @property {string} text
 */

/**
 * A comment block under construction while walking a patch.
 * @typedef {object} PendingGroup
 * @property {number} start
 * @property {number} end
 * @property {number} indent
 * @property {string[]} lines
 */

/**
 * The subset of the `actions/github-script` Octokit client this script uses.
 * The local shim in this file implements exactly these four call shapes.
 * @typedef {object} GithubClient
 * @property {(route: unknown, params: Record<string, unknown>) => Promise<any[]>} paginate
 * @property {(query: string, variables?: Record<string, unknown>) => Promise<any>} graphql
 * @property {{pulls: {listFiles: unknown, createReviewComment: (params: Record<string, unknown>) => Promise<unknown>}}} rest
 */

/**
 * The subset of `@actions/core` this script uses.
 * @typedef {object} CoreLike
 * @property {(message: string) => void} info
 * @property {(message: string) => void} warning
 */

/**
 * The subset of the workflow context this script reads.
 * @typedef {object} ContextLike
 * @property {{owner: string, repo: string}} repo
 * @property {{pull_request: {number: number, head: {sha: string}}}} payload
 */

/* -------------------------------------------------------------- heuristics */

const GO_FILE = /\.go$/;
const MIN_LINES = 3;

// These are deliberate escape hatches for comments whose length
// exists because the constraint itself matters, rather than
// because the implementation needs narrating.
const ALLOWED_MARKER = /\b(?:SAFETY|INVARIANT|PROTOCOL|COMPAT):/;

/**
 * @param {string} line
 * @returns {boolean}
 */
function isCommentLine(line) {
	const t = line.trimStart();

	if (t.startsWith('//')) return true;
	if (t.startsWith('/*')) return true;
	if (t === '*' || t === '*/' || t.startsWith('* ')) return true;

	return false;
}

/**
 * @param {string} line
 * @returns {number}
 */
function indentation(line) {
	return line.match(/^[\t ]*/)?.[0].length ?? 0;
}

/**
 * @param {string} line
 * @returns {string}
 */
function stripCommentPrefix(line) {
	return line
		.trimStart()
		.replace(/^\/\/\s?/, '')
		.replace(/^\/\*\s?/, '')
		.replace(/^\*\s?/, '')
		.trim();
}

// Go requires / encourages declaration comments. Those are not
// what this workflow is policing.
//
// Top-level comment groups are treated as declaration/package
// documentation. Indented docs for exported fields, methods,
// constants, etc. are recognized by the conventional:
//
//   // Foo ...
//   Foo ...
//
// shape.
/**
 * @param {PendingGroup} cur
 * @param {string} nextLine
 * @returns {boolean}
 */
function looksLikeGoDoc(cur, nextLine) {
	if (cur.indent === 0) return true;
	if (!nextLine) return false;

	const first = stripCommentPrefix(cur.lines[0]);
	const next = nextLine.trim();

	const declaration = /^([A-Z][A-Za-z0-9_]*)\b/.exec(next);
	if (!declaration) return false;

	const name = declaration[1];
	return (
		first === name
		|| first.startsWith(`${name} `)
		|| first.startsWith(`${name}.`)
		|| first.startsWith(`${name},`)
	);
}

/**
 * @param {string} path
 * @param {string} patch
 * @returns {Group[]}
 */
function groupsFromPatch(path, patch) {
	/** @type {Group[]} */
	const out = [];

	let newLine = 0;
	/** @type {PendingGroup | null} */
	let cur = null;

	/** @param {string} nextLine */
	const flush = nextLine => {
		if (cur && cur.lines.length >= MIN_LINES) {
			const text = cur.lines.join('\n');

			if (
				!ALLOWED_MARKER.test(text)
				&& !looksLikeGoDoc(cur, nextLine)
			) {
				out.push({
					path,
					start: cur.start,
					end: cur.end,
					text,
				});
			}
		}

		cur = null;
	};

	for (const raw of patch.split('\n')) {
		if (raw.startsWith('@@')) {
			flush('');

			const match = /\+(\d+)/.exec(raw);
			newLine = match ? Number.parseInt(match[1], 10) : 1;
			continue;
		}

		if (raw.startsWith('+')) {
			const content = raw.slice(1);

			if (isCommentLine(content)) {
				if (cur) {
					cur.end = newLine;
					cur.lines.push(content);
				} else {
					cur = {
						start: newLine,
						end: newLine,
						indent: indentation(content),
						lines: [content],
					};
				}
			} else {
				flush(content);
			}

			newLine++;
			continue;
		}

		if (raw.startsWith('-')) {
			flush('');
			continue;
		}

		if (raw.startsWith('\\')) {
			// "\ No newline at end of file"
			continue;
		}

		// Context line. It is useful as lookahead for detecting
		// declaration documentation immediately following an added
		// comment block.
		const content = raw.startsWith(' ') ? raw.slice(1) : raw;
		flush(content);
		newLine++;
	}

	flush('');
	return out;
}

/**
 * Marker key identifying one finding across runs. Existing review threads
 * carry this exact derivation; changing it orphans them.
 * @param {Group} group
 * @returns {string}
 */
const keyFor = group => `${group.path}:${createHash('sha256').update(group.text).digest('hex').slice(0, 12)}`;

/**
 * @param {Group} group
 * @returns {string}
 */
const bodyFor = group =>
	`<!-- statute-comment-cop:${keyFor(group)} -->\n`
	+ `This comment is doing too much of the code's job. `
	+ `Prefer making the ownership, state, or control flow explicit in code and keep only the non-obvious constraint here.\n\n`
	+ `Paragraph-length implementation comments are reserved for genuine `
	+ `\`SAFETY:\`, \`INVARIANT:\`, \`PROTOCOL:\`, or \`COMPAT:\` constraints.`;

/**
 * @param {unknown} error
 * @returns {string}
 */
function errorMessage(error) {
	return error instanceof Error ? error.message : String(error);
}

/* ------------------------------------------------------------------ runner */

/**
 * Scan the pull request diff, resolve stale Comment Cop threads, and post one
 * review comment per new finding.
 *
 * @param {object} options
 * @param {GithubClient} options.github
 * @param {ContextLike} options.context
 * @param {CoreLike} options.core
 * @param {boolean} [options.dryRun] report findings without calling any mutating API
 * @returns {Promise<void>}
 */
export default async function run({ github, context, core, dryRun = false }) {
	const owner = context.repo.owner;
	const repo = context.repo.repo;
	const pull_number = context.payload.pull_request.number;
	const headSha = context.payload.pull_request.head.sha;

	const files = await github.paginate(
		github.rest.pulls.listFiles,
		{
			owner,
			repo,
			pull_number,
			per_page: 100,
		},
	);

	/** @type {Group[]} */
	const groups = [];

	for (const file of files) {
		if (file.status === 'removed') continue;
		if (!GO_FILE.test(file.filename)) continue;

		// Never police vendored code if it appears in a PR.
		if (
			file.filename.startsWith('vendor/')
			|| file.filename.includes('/vendor/')
		) {
			continue;
		}

		if (!file.patch) {
			core.warning(
				`No patch available for ${file.filename}; skipping comment scan.`,
			);
			continue;
		}

		for (const group of groupsFromPatch(file.filename, file.patch)) {
			groups.push(group);
		}
	}

	const presentKeys = new Set(groups.map(keyFor));

	if (dryRun) {
		core.info(`[dry-run] ${groups.length} comment group(s) currently present:`);
		for (const group of groups) {
			core.info(`[dry-run]   ${group.path}:${group.start}-${group.end} key=${keyFor(group)}`);
			for (const line of group.text.split('\n')) core.info(`[dry-run]     | ${line}`);
		}
	}

	// Existing Comment Cop review threads are used both to avoid
	// duplicate comments and to resolve findings automatically once
	// their corresponding block disappears from the current diff.
	const threads = [];

	{
		const query = `
			query(
				$owner: String!
				$repo: String!
				$pr: Int!
				$after: String
			) {
				repository(owner: $owner, name: $repo) {
					pullRequest(number: $pr) {
						reviewThreads(first: 100, after: $after) {
							pageInfo {
								hasNextPage
								endCursor
							}
							nodes {
								id
								isResolved
								comments(first: 1) {
									nodes {
										body
									}
								}
							}
						}
					}
				}
			}
		`;

		/** @type {string | null} */
		let after = null;

		for (;;) {
			const response = await github.graphql(query, {
				owner,
				repo,
				pr: pull_number,
				after,
			});

			const page = response.repository.pullRequest.reviewThreads;

			threads.push(...page.nodes);

			if (!page.pageInfo.hasNextPage) break;
			after = page.pageInfo.endCursor;
		}
	}

	/** @type {Set<string>} */
	const seenKeys = new Set();
	/** @type {string[]} */
	const toResolve = [];

	for (const thread of threads) {
		const body = thread.comments.nodes[0]?.body ?? '';

		const match = /<!-- statute-comment-cop:([^>\s]+) -->/.exec(body);

		if (!match) continue;

		const key = match[1];
		seenKeys.add(key);

		if (dryRun) {
			core.info(
				`[dry-run] existing cop thread ${thread.id} key=${key} `
					+ `resolved=${thread.isResolved} stillPresent=${presentKeys.has(key)}`,
			);
		}

		if (!thread.isResolved && !presentKeys.has(key)) {
			toResolve.push(thread.id);
		}
	}

	if (toResolve.length > 0) {
		const mutation = `
			mutation($id: ID!) {
				resolveReviewThread(input: { threadId: $id }) {
					thread {
						id
					}
				}
			}
		`;

		let resolved = 0;
		let failed = 0;

		for (const id of toResolve) {
			if (dryRun) {
				core.info(`[dry-run] would resolve stale thread ${id}`);
				continue;
			}

			try {
				await github.graphql(mutation, { id });
				resolved++;
			} catch (error) {
				failed++;
				core.warning(
					`resolveReviewThread failed for ${id}: ${errorMessage(error)}`,
				);
			}
		}

		if (!dryRun) {
			core.info(
				`Stale Comment Cop threads: ${resolved} resolved, ${failed} failed.`,
			);
		}
	}

	const fresh = groups.filter(group => !seenKeys.has(keyFor(group)));

	if (fresh.length === 0) {
		core.info(
			`No new comment groups to flag (${groups.length} currently present).`,
		);
		return;
	}

	let posted = 0;

	for (const group of fresh) {
		/** @type {Record<string, unknown>} */
		const params = {
			owner,
			repo,
			pull_number,
			commit_id: headSha,
			path: group.path,
			line: group.end,
			side: 'RIGHT',
			body: bodyFor(group),
		};

		if (group.start < group.end) {
			params.start_line = group.start;
			params.start_side = 'RIGHT';
		}

		if (dryRun) {
			core.info(
				`[dry-run] would comment on ${group.path}:${group.start}-${group.end} key=${keyFor(group)}`,
			);
			continue;
		}

		try {
			await github.rest.pulls.createReviewComment(params);
			posted++;
		} catch (error) {
			core.warning(
				`createReviewComment failed for ${group.path}:${group.start}-${group.end}: ${errorMessage(error)}`,
			);
		}
	}

	if (dryRun) {
		core.info(`[dry-run] ${fresh.length} new finding(s); nothing posted.`);
		return;
	}

	core.info(`Posted ${posted} Comment Cop review comment(s).`);
}

/* ------------------------------------------------------------- local entry */

/**
 * Minimal fetch-based stand-in for the github-script Octokit client. It
 * implements only the four call shapes above; anything else is out of scope.
 *
 * @param {string} token
 * @returns {GithubClient}
 */
function localClient(token) {
	const API = 'https://api.github.com';

	/**
	 * @param {string} path
	 * @param {RequestInit} [init]
	 * @returns {Promise<any>}
	 */
	async function request(path, init) {
		const response = await fetch(`${API}${path}`, {
			...init,
			headers: {
				accept: 'application/vnd.github+json',
				authorization: `Bearer ${token}`,
				'content-type': 'application/json',
				'x-github-api-version': '2022-11-28',
				'user-agent': 'statute-comment-cop',
			},
		});

		if (!response.ok) {
			throw new Error(`${init?.method ?? 'GET'} ${path} -> ${response.status} ${await response.text()}`);
		}

		return response.json();
	}

	/** @param {Record<string, any>} params */
	const listFiles = params =>
		request(
			`/repos/${params.owner}/${params.repo}/pulls/${params.pull_number}/files`
				+ `?per_page=${params.per_page ?? 100}&page=${params.page ?? 1}`,
		);

	return {
		rest: {
			pulls: {
				listFiles,
				createReviewComment: params =>
					request(`/repos/${params.owner}/${params.repo}/pulls/${params.pull_number}/comments`, {
						method: 'POST',
						body: JSON.stringify(params),
					}),
			},
		},

		paginate: async (route, params) => {
			if (route !== listFiles) throw new Error('local shim paginates pulls.listFiles only');

			/** @type {any[]} */
			const all = [];
			const perPage = Number(params.per_page ?? 100);

			for (let page = 1;; page++) {
				const batch = await listFiles({ ...params, page });
				all.push(...batch);
				if (batch.length < perPage) break;
			}

			return all;
		},

		graphql: async (query, variables) => {
			const response = await request('/graphql', {
				method: 'POST',
				body: JSON.stringify({ query, variables: variables ?? {} }),
			});

			if (response.errors) throw new Error(JSON.stringify(response.errors));
			return response.data;
		},
	};
}

/**
 * @returns {Promise<void>}
 */
async function main() {
	const argv = process.argv.slice(2);
	const apply = argv.includes('--apply');
	const positional = argv.filter(arg => !arg.startsWith('--'));

	const slug = positional[0];
	const number = Number(positional[1]);

	if (!slug || !slug.includes('/') || !Number.isInteger(number)) {
		console.error('usage: node scripts/comment-cop.mjs <owner>/<repo> <pr-number> [--apply]');
		process.exitCode = 2;
		return;
	}

	const token = process.env.GITHUB_TOKEN;
	if (!token) {
		console.error('GITHUB_TOKEN is required (try: GITHUB_TOKEN=$(gh auth token) ...)');
		process.exitCode = 2;
		return;
	}

	const [owner, repo] = slug.split('/');
	const github = localClient(token);

	/** @type {any} */
	const pull = await github.graphql(
		`query($owner: String!, $repo: String!, $pr: Int!) {
			repository(owner: $owner, name: $repo) {
				pullRequest(number: $pr) { headRefOid }
			}
		}`,
		{ owner, repo, pr: number },
	);

	/** @type {CoreLike} */
	const core = {
		info: message => console.log(message),
		warning: message => console.warn(`warning: ${message}`),
	};

	if (!apply) core.info(`DRY RUN: scanning ${slug}#${number}, nothing will be posted or resolved.`);

	await run({
		github,
		core,
		dryRun: !apply,
		context: {
			repo: { owner, repo },
			payload: {
				pull_request: {
					number,
					head: { sha: pull.repository.pullRequest.headRefOid },
				},
			},
		},
	});
}

if (import.meta.main) await main();
