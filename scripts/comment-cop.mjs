#!/usr/bin/env node
// @ts-check
// Comment Cop: flags prose a pull request adds to comments and Markdown,
// and resolves its own stale review threads.
//
// Three entry points, one scanner:
//
//   - GitHub Actions: `.github/workflows/comment-cop.yml` imports this file
//     and calls the default export with the `actions/github-script` objects.
//   - Working tree (the default): `node scripts/comment-cop.mjs`, or
//     `make comment-cop`, or `... <base-ref>` to compare against something
//     other than the merge base with master. Scans the local diff,
//     uncommitted and untracked files included. No token, no network;
//     exits 1 on any finding, so it gates a push before the PR exists.
//   - A pull request: `node scripts/comment-cop.mjs kjanat/statute 72`
//     Requires Node >= 24.2 (for `import.meta.main`) or Bun, and a
//     `GITHUB_TOKEN` in the environment.
//
// Pull-request runs are DRY RUN unless `--apply` is passed: they scan, print
// every finding and every thread that would be resolved, and call no mutating
// API. `--local` never has anything to post and ignores `--apply`.

import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';

/* ------------------------------------------------------------------- types */

/**
 * One flagged comment block, located in the head-side file.
 * @typedef {object} Group
 * @property {string} path
 * @property {number} start
 * @property {number} end
 * @property {string} text
 * @property {string[]} reasons
 */

/**
 * A comment block under construction while walking a patch.
 * @typedef {object} PendingGroup
 * @property {number} start
 * @property {number} end
 * @property {Lang} lang
 * @property {boolean} topLevel
 * @property {boolean} doc
 * @property {boolean} maybeDoc
 * @property {string[]} lines
 */

/**
 * How a file carries prose. `go` and `js` both use slash comments but only
 * Go has a doc-comment contract; `hash` covers YAML, shell, and friends;
 * `md` has no comment wrapper at all, the paragraph is the block.
 * @typedef {'go' | 'js' | 'hash' | 'md'} Lang
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

// Prose lives in comments and in Markdown alike. Formats with no comment
// syntax, machine-owned files, key material and path lists are skipped.
/** @type {Array<[RegExp, Lang]>} */
const LANGS = [
	[/\.go$/, 'go'],
	[/\.(?:mjs|cjs|js|ts|jsonc)$/, 'js'],
	[/\.(?:ya?ml|py|sh|bash|toml)$/, 'hash'],
	[/(?:^|\/)(?:Makefile|Dockerfile)(?:\.[\w.-]+)?$/, 'hash'],
	[/\.md$/, 'md'],
];

/**
 * How this path carries prose, or null when it carries none.
 * @param {string} path
 * @returns {Lang | null}
 */
function langFor(path) {
	for (const [pattern, lang] of LANGS) {
		if (pattern.test(path)) return lang;
	}
	return null;
}

// Length measures ordinary comments only. Doc comments are read without the
// code at hand, where line count says nothing about quality.
const MIN_LINES = 3;

// go.dev/doc/comment: a doc comment appears immediately before a top-level
// package, const, func, type, or var declaration, with no blank line between.
const TOP_LEVEL_DECL = /^(?:package|const|func|type|var)\b/;

// Matched against the block's joined text, so a wrapped construction still
// reads as one sentence. These apply at any length and any position.
/** @type {Array<[string, RegExp]>} */
const TELLS = [
	['em dash', /[—–]/],
	['"X, not Y"', /,[\s]+not\s+\S/],
	['"X rather than Y"', /\brather than\b/i],
	['"X instead of Y"', /\b(?:instead of|as opposed to)\b/i],
	['"not just X but Y"', /\bnot (?:just|merely|only|because)\b[^.]{0,80}?\bbut\b/i],
	[
		'emphatic cleft',
		/\b(?:which|that) is (?:what|why|how)\b|\bexactly (?:what|why|how|the)\b/i,
	],
	[
		'filler phrase',
		/\b(?:in other words|it(?:'s| is) (?:worth noting|important to note)|that said|under the hood|at its core|(?:simply put|put simply)|in short|in essence|bottom line|needless to say|when it comes to|at the end of the day|think of (?:it|this) as|no more,? no less|(?:that|which) is to say|here(?:'s| is) (?:why|the thing)|the (?:whole|entire) point|the key (?:insight|takeaway))\b/i,
	],
	[
		'inflated diction',
		/\b(?:leverag(?:e|es|ing)|utiliz(?:e|es|ing)|seamless(?:ly)?|delv(?:e|es|ing)|myriad|plethora|robust|comprehensive(?:ly)?|crucial(?:ly)?|vital(?:ly)?|elegant(?:ly)?|powerful(?:ly)?|intuitive(?:ly)?|nuanced|holistic|granular|meticulous|facilitat(?:e|es|ing)|streamlin(?:e|es|ing)|empower(?:s|ing)?|cutting[-\s]edge|state[-\s]of[-\s]the[-\s]art|arguably|essentially|fundamentally|a wealth of)\b/i,
	],
	[
		'connective glue',
		/\b(?:moreover|furthermore|conversely|as such|it turns out|notably|importantly)\b/i,
	],
	[
		'counterfactual justification',
		/\bso\b[^.]{0,60}\b(?:cannot|can't|could not|never|would)\b|\bwithout\b[^.]{0,70}\bwould\b|\bwould otherwise\b|\botherwise\b[^.]{0,70}\bwould\b|\bso that\b|\b(?:which|that) (?:prevents|keeps|stops)\b/i,
	],
	// Not typed by hand. These arrive by paste.
	['paste artifact', /[“”‘’]|[\u00A0\u00AD\u200B-\u200D\uFEFF]/],
];

// A comment about the character has to be able to print it.
const DASH_AS_SUBJECT = /[`'"][—–][`'"]|\b(?:em|en)[-\s]dash|U\+201[34]/i;

const FENCE = /^\s*(?:```|~~~)/;

// A list, heading or table row is its own block. Markdown puts no blank line
// between items, and a whole list reported as one finding is unreadable.
const MD_ITEM = /^\s*(?:[-*+]\s|\d+[.)]\s|#{1,6}\s|\||>\s)/;

const JSDOC_TAG = /^\s*\*\s*@\w+/;

/**
 * @param {string} line
 * @param {Lang} lang
 * @returns {boolean}
 */
function isCommentLine(line, lang) {
	const t = line.trimStart();

	if (lang === 'hash') {
		return t.startsWith('#') && !t.startsWith('#!');
	}

	if (t.startsWith('//')) return true;
	if (t.startsWith('/*')) return true;
	if (t === '*' || t === '*/' || t.startsWith('* ')) return true;

	return false;
}

/**
 * Whether this block is documentation the language's own tooling reads: a Go
 * doc comment, or a JSDoc block. Both are written for someone without the
 * code in front of them, so neither is measured by length.
 * @param {PendingGroup} cur
 * @param {string} nextLine
 * @returns {boolean}
 */
function isDocBlock(cur, nextLine) {
	if (cur.doc) return true;

	// A `*` fragment is either JSDoc past its `/**` or an ordinary block
	// comment past its `/*`. A tag tells the two apart.
	if (cur.maybeDoc && cur.lines.some(line => JSDOC_TAG.test(line))) return true;

	// TOP_LEVEL_DECL matches JavaScript's const and var too, hence the guard.
	return cur.lang === 'go' && cur.topLevel && TOP_LEVEL_DECL.test(nextLine);
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
		.replace(/^#\s?/, '')
		.replace(/^\*\s?/, '')
		.trim();
}

/**
 * Every reason this block is flagged, in reporting order.
 * @param {PendingGroup} cur
 * @param {string} nextLine
 * @returns {string[]}
 */
function reasonsFor(cur, nextLine) {
	const reasons = [];

	// Length measures a comment against the code beside it. A Markdown
	// paragraph has no code beside it, so there is nothing to measure.
	if (
		cur.lang !== 'md'
		&& cur.lines.length >= MIN_LINES
		&& !isDocBlock(cur, nextLine)
	) {
		reasons.push(`${cur.lines.length} lines`);
	}

	const text = cur.lang === 'md'
		? cur.lines.join(' ')
		: cur.lines.map(stripCommentPrefix).join(' ');

	for (const [name, pattern] of TELLS) {
		if (!pattern.test(text)) continue;
		if (name === 'em dash' && DASH_AS_SUBJECT.test(text)) continue;
		reasons.push(name);
	}

	return reasons;
}

/**
 * @param {string} path
 * @param {string} patch
 * @returns {Group[]}
 */
function groupsFromPatch(path, patch) {
	/** @type {Group[]} */
	const out = [];

	const lang = langFor(path) ?? 'js';
	const md = lang === 'md';
	let newLine = 0;
	let inFence = false;
	/** @type {PendingGroup | null} */
	let cur = null;

	// A Markdown block is a paragraph: unblank, unfenced, unindented.
	/** @param {string} content */
	const inBlock = content => {
		if (!md) return isCommentLine(content, lang);
		if (FENCE.test(content)) {
			inFence = !inFence;
			return false;
		}
		return !inFence && content.trim() !== '' && !/^(?: {4}|\t)/.test(content);
	};

	/** @param {string} nextLine */
	const flush = nextLine => {
		if (cur) {
			const reasons = reasonsFor(cur, nextLine);

			if (reasons.length > 0) {
				out.push({
					path,
					start: cur.start,
					end: cur.end,
					text: cur.lines.join('\n'),
					reasons,
				});
			}
		}

		cur = null;
	};

	for (const raw of patch.split('\n')) {
		if (raw.startsWith('@@')) {
			flush('');

			// Hunks are discontiguous. Carrying fence state across one lets a
			// single unclosed fence blank the rest of the file.
			inFence = false;

			const match = /\+(\d+)/.exec(raw);
			newLine = match ? Number.parseInt(match[1], 10) : 1;
			continue;
		}

		if (raw.startsWith('+')) {
			const content = raw.slice(1);

			if (inBlock(content)) {
				const t = content.trimStart();

				// A JSDoc opener and a Markdown item start their own block.
				if (cur && (md ? MD_ITEM.test(content) : t.startsWith('/**'))) {
					flush(content);
				}

				if (cur) {
					cur.end = newLine;
					cur.lines.push(content);
				} else {
					cur = {
						start: newLine,
						end: newLine,
						lang,
						topLevel: !/^[\t ]/.test(content),
						doc: t.startsWith('/**'),
						// A hunk can open mid-block, past the `/**`.
						maybeDoc: t.startsWith('*'),
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

		// Context line: ends the run, and is the declaration lookahead.
		const content = raw.startsWith(' ') ? raw.slice(1) : raw;
		if (md && FENCE.test(content)) inFence = !inFence;
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
	+ `Flagged for: ${group.reasons.join(', ')}.\n\n`
	+ `This comment is doing too much of the code's job. `
	+ `Prefer making the ownership, state, or control flow explicit in code and keep only the non-obvious constraint here.`;

/**
 * @param {unknown} error
 * @returns {string}
 */
function errorMessage(error) {
	return error instanceof Error ? error.message : String(error);
}

/* -------------------------------------------------------------- local scan */

/** @type {(args: string[]) => string} */
const git = args => execFileSync('git', args, { encoding: 'utf8', maxBuffer: 1 << 28 });

/**
 * The merge base with the default branch, whichever remote-tracking or local
 * ref exists here.
 * @returns {string}
 */
function defaultBase() {
	for (const ref of ['origin/master', 'master']) {
		try {
			// A probe: a missing ref is expected, so keep git's stderr quiet.
			return execFileSync('git', ['merge-base', 'HEAD', ref], {
				encoding: 'utf8',
				stdio: ['ignore', 'pipe', 'pipe'],
			}).trim();
		} catch {
			continue;
		}
	}
	throw new Error('no origin/master or master to compare against; pass a base ref explicitly');
}

/**
 * Diff one untracked file as wholly added, so a brand-new file is scanned the
 * way the pull-request lister would see it.
 * @param {string} name
 * @returns {string}
 */
function untrackedPatch(name) {
	try {
		return git(['diff', '--no-index', '--', '/dev/null', name]);
	} catch (error) {
		// --no-index exits non-zero whenever the inputs differ, which is always.
		const stdout = /** @type {{stdout?: unknown}} */ (error)?.stdout;
		return typeof stdout === 'string' ? stdout : '';
	}
}

/** @type {(name: string) => boolean} */
const scannable = name =>
	name !== ''
	&& langFor(name) !== null
	&& !name.startsWith('vendor/')
	&& !name.includes('/vendor/');

/** @type {(out: string) => string[]} */
const lines = out => out.split('\n').map(line => line.trim()).filter(name => name !== '');

/**
 * Scan the working tree against a base ref and print every finding.
 * @param {string} [base]
 * @returns {number} the process exit code
 */
function scanLocal(base) {
	const from = base ?? defaultBase();

	const tracked = lines(git(['diff', '--name-only', '--diff-filter=d', from, '--'])).filter(scannable);
	const untracked = lines(git(['ls-files', '--others', '--exclude-standard'])).filter(scannable);

	/** @type {Group[]} */
	const groups = [];

	for (const name of tracked) {
		groups.push(...groupsFromPatch(name, git(['diff', '-U3', from, '--', name])));
	}
	for (const name of untracked) {
		groups.push(...groupsFromPatch(name, untrackedPatch(name)));
	}

	const scope = `${tracked.length + untracked.length} file(s) vs ${from.slice(0, 12)}`;
	if (groups.length === 0) {
		console.log(`comment-cop: clean (${scope}).`);
		return 0;
	}

	for (const group of groups) {
		console.log(`${group.path}:${group.start}-${group.end}  [${group.reasons.join(', ')}]`);
		console.log(`${group.text}\n`);
	}

	console.log(
		`comment-cop: ${groups.length} finding(s) (${scope}).\n`
			+ 'Keep only the non-obvious constraint.',
	);
	return 1;
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
		if (langFor(file.filename) === null) continue;

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

const USAGE = `comment-cop — flag paragraph-length implementation comments in Go source.

usage:
  comment-cop.mjs [<base-ref>]                 scan the working tree (default)
  comment-cop.mjs <owner>/<repo> <pr-number>   scan a pull request
  comment-cop.mjs --help

working tree:
  Diffs against <base-ref>, or the merge base with master when omitted.
  Uncommitted and untracked files are included. No token, no network.
  Exits 1 when anything is flagged, so it can gate a commit or push.

pull request:
  Needs GITHUB_TOKEN (try: GITHUB_TOKEN=$(gh auth token) ...). Dry run by
  default: it prints findings and the threads it would resolve, and posts
  nothing until --apply is passed.

options:
  --local     force working-tree mode even when a repo and number are given
  --apply     pull-request mode only: actually post and resolve
  -h, --help  show this help

files:
  Go, JS/TS, JSONC, YAML, Python, shell, TOML, Makefiles and Dockerfiles
  contribute their comments; Markdown contributes its paragraphs, one per
  list item, heading or table row. Fenced and indented code is skipped,
  as are formats with no comment syntax, machine-owned files, key
  material and path lists.

length rule:
  An ordinary comment of three or more lines is flagged. Go doc comments
  and JSDoc blocks are exempt: they are read without the code at hand,
  where line count says nothing about quality.

style rule:
  Any comment is flagged, whatever its length or position, for an em/en
  dash, a contrast construction ("X, not Y", "X rather than Y", "X
  instead of Y", "not just X but Y"), an emphatic cleft ("which is what",
  "exactly the"), counterfactual justification ("so X cannot Y",
  "without X, Y would"), stock filler, connective glue, inflated diction,
  or a paste artifact such as a curly quote or a zero-width space. A dash
  passes when the comment is about the character itself, shown by quoting
  it, naming it, or giving its code point.`;

/**
 * @returns {Promise<void>}
 */
async function main() {
	const argv = process.argv.slice(2);
	const flags = argv.filter(arg => arg.startsWith('-'));
	const positional = argv.filter(arg => !arg.startsWith('-'));

	if (flags.includes('--help') || flags.includes('-h')) {
		console.log(USAGE);
		return;
	}

	const unknown = flags.filter(flag => !['--local', '--apply'].includes(flag));
	if (unknown.length > 0) {
		console.error(`unknown option: ${unknown.join(' ')}\n\n${USAGE}`);
		process.exitCode = 2;
		return;
	}

	const apply = flags.includes('--apply');
	const slug = positional[0];
	const number = Number(positional[1]);
	// Scanning the working tree is the common case, so it is the default;
	// naming a repo and a PR number is what selects the pull-request mode.
	const pullRequest = slug !== undefined && slug.includes('/') && Number.isInteger(number);

	if (flags.includes('--local') || !pullRequest) {
		if (positional.length > 1 || (slug !== undefined && Number.isInteger(Number(slug)))) {
			console.error(`not a base ref, and not <owner>/<repo> <pr-number>\n\n${USAGE}`);
			process.exitCode = 2;
			return;
		}
		try {
			process.exitCode = scanLocal(slug);
		} catch (error) {
			console.error(errorMessage(error));
			process.exitCode = 2;
		}
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
