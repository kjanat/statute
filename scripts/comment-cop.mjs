#!/usr/bin/env node
// @ts-check

import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

/** @typedef {'go' | 'js' | 'hash' | 'md'} Lang */
/** @typedef {{path: string, start: number, end: number, text: string, reasons: string[]}} Group */
/** @typedef {{start: number, end: number, lang: Lang, topLevel: boolean, doc: boolean, lines: string[]}} PendingGroup */
/** @typedef {{id: string, isResolved: boolean, path: string, comments: {nodes: Array<{body: string, viewerDidAuthor: boolean}>}}} ReviewThread */
/** @typedef {{repository: {pullRequest: {reviewThreads: {pageInfo: {hasNextPage: boolean, endCursor: string | null}, nodes: ReviewThread[]}}}}} ReviewThreadsResponse */
/** @typedef {{owner: string, repo: string, pull_number: number, commit_id?: string, event?: string, body?: string, comments: Array<{path: string, line: number, side: string, body: string, start_line?: number, start_side?: string}>}} ReviewParams */
/** @typedef {{paginate: (route: unknown, params: Record<string, unknown>) => Promise<any[]>, graphql: (query: string, variables?: Record<string, unknown>) => Promise<any>, request: (url: string, options?: any) => Promise<{data: any}>, rest: {pulls: {listFiles: unknown, createReview: (params: ReviewParams) => Promise<unknown>}}}} GithubClient */
/** @typedef {{info: (message: string) => void, warning: (message: string) => void}} CoreLike */
/** @typedef {{repo: {owner: string, repo: string}, payload: {pull_request: {number: number, head: {sha: string}}}}} ContextLike */
/** @typedef {{github: GithubClient, context: ContextLike, core: CoreLike, dryRun?: boolean}} RunArguments */
/** @typedef {{character: '`' | '~', length: number} | null} Fence */

/** @type {Array<[RegExp, Lang]>} */
const LANGS = [
	[/\.go$/, 'go'],
	[/\.(?:mjs|cjs|js|ts|jsonc)$/, 'js'],
	[/\.(?:ya?ml|py|sh|bash|toml)$/, 'hash'],
	[/(?:^|\/)(?:Makefile|Dockerfile)(?:\.[\w.-]+)?$/, 'hash'],
	[/\.md$/, 'md'],
];

const CONTRAST_GUIDANCE =
	'Check whether the comparison explains a real constraint. If it does, keep it; otherwise describe the chosen behavior directly.';

/** @type {Array<[string, RegExp, string]>} */
const TELLS = [
	[
		'em dash',
		/[—–]/,
		'For ordinary prose, consider a comma, colon, parentheses, or separate sentence. Preserve punctuation that is part of quoted material.',
	],
	['"X, not Y"', /,[\s]+not\s+\S/, CONTRAST_GUIDANCE],
	['"X rather than Y"', /\brather than\b/i, CONTRAST_GUIDANCE],
	['"X instead of Y"', /\b(?:instead of|as opposed to)\b/i, CONTRAST_GUIDANCE],
	['"not just X but Y"', /\bnot (?:just|merely|only|because)\b[^.]{0,80}?\bbut\b/i, CONTRAST_GUIDANCE],
	[
		'emphatic cleft',
		/\b(?:which|that) is (?:what|why|how)\b|\bexactly (?:what|why|how|the)\b/i,
		'Consider stating the behavior or reason directly. Keep the emphasis if it carries a meaningful distinction.',
	],
	[
		'filler phrase',
		/\b(?:in other words|it(?:'s| is) (?:worth noting|important to note)|that said|under the hood|at its core|(?:simply put|put simply)|in short|in essence|bottom line|needless to say|when it comes to|at the end of the day|think of (?:it|this) as|no more,? no less|(?:that|which) is to say|here(?:'s| is) (?:why|the thing)|the (?:whole|entire) point|the key (?:insight|takeaway))\b/i,
		'Check whether the introductory phrase adds meaning. If the explanation reads clearly without it, omit the phrase.',
	],
	[
		'inflated diction',
		/\b(?:leverag(?:e|es|ing)|utiliz(?:e|es|ing)|seamless(?:ly)?|delv(?:e|es|ing)|myriad|plethora|robust|comprehensive(?:ly)?|crucial(?:ly)?|vital(?:ly)?|elegant(?:ly)?|powerful(?:ly)?|intuitive(?:ly)?|nuanced|holistic|granular|meticulous|facilitat(?:e|es|ing)|streamlin(?:e|es|ing)|empower(?:s|ing)?|cutting[-\s]edge|state[-\s]of[-\s]the[-\s]art|arguably|essentially|fundamentally|a wealth of)\b/i,
		'Consider a plain, precise term. Keep the existing word if it has a specific technical meaning here.',
	],
	[
		'connective glue',
		/\b(?:moreover|furthermore|conversely|as such|it turns out|notably|importantly)\b/i,
		'Check whether the transition helps connect the surrounding points. It can be omitted when that connection is already clear.',
	],
	[
		'counterfactual justification',
		/\bso\b[^.]{0,60}\b(?:cannot|can't|could not|never|would)\b|\bwithout\b[^.]{0,70}\bwould\b|\bwould otherwise\b|\botherwise\b[^.]{0,70}\bwould\b|\bso that\b|\b(?:which|that) (?:prevents|keeps|stops)\b/i,
		'An explanation of a consequence or failure mode can be useful. Keep it when it documents a non-obvious constraint; otherwise state the behavior directly.',
	],
	[
		'paste artifact',
		/[“”‘’]|[\u00A0\u00AD\u200B-\u200D\uFEFF]/,
		'Check typographic quotes and nonstandard whitespace for accidental pasted characters. Preserve intentional examples and quotations.',
	],
];

const TOP_LEVEL_DECL = /^(?:package|const|func|type|var)\b/;
const DASH_AS_SUBJECT = /[`'"][—–][`'"]|\b(?:em|en)[-\s]dash|U\+201[34]/i;
const MD_ITEM = /^\s*(?:[-*+]\s|\d+[.)]\s|#{1,6}\s|\||>\s)/;

/** @param {string} path @returns {Lang | null} */
function langFor(path) {
	for (const [pattern, lang] of LANGS) {
		if (pattern.test(path)) return lang;
	}
	return null;
}

/** @param {string} line @param {Lang} lang */
function isCommentLine(line, lang) {
	const text = line.trimStart();
	if (lang === 'hash') return text.startsWith('#') && !text.startsWith('#!');
	return text.startsWith('//')
		|| text.startsWith('/*')
		|| text === '*'
		|| text === '*/'
		|| text.startsWith('* ');
}

/** @param {string} line */
function stripCommentPrefix(line) {
	return line
		.trimStart()
		.replace(/^\/\/\s?/, '')
		.replace(/^\/\*\*?\s?/, '')
		.replace(/^#\s?/, '')
		.replace(/^\*\s?/, '')
		.trim();
}

/** @param {PendingGroup} group @param {string} nextLine @param {string[] | undefined} sourceLines */
function isDocBlock(group, nextLine, sourceLines) {
	if (
		group.doc || (group.lines[0]?.trimStart().startsWith('*') && group.lines.some(line => /^\s*\*\s*@\w+/.test(line)))
	) return true;
	if (group.lang !== 'go') return false;
	let firstLine = group.lines[0];
	if (sourceLines !== undefined) {
		let before = group.start - 1;
		let after = group.end;
		while (before > 0 && isCommentLine(sourceLines[before - 1], 'go')) before--;
		while (after < sourceLines.length && isCommentLine(sourceLines[after], 'go')) after++;
		firstLine = sourceLines[before] ?? firstLine;
		nextLine = sourceLines[after] ?? nextLine;
	}
	if (group.topLevel && TOP_LEVEL_DECL.test(nextLine)) return true;
	const field = /^\s+([A-Za-z_]\w*)\s+(?:[A-Za-z_*]|\[)/.exec(nextLine)?.[1];
	return field !== undefined && stripCommentPrefix(firstLine).startsWith(`${field} `);
}

/** @param {PendingGroup} group @param {string} nextLine @param {string[] | undefined} sourceLines */
function reasonsFor(group, nextLine, sourceLines) {
	const reasons = [];
	if (group.lang !== 'md' && group.lines.length >= 3 && !isDocBlock(group, nextLine, sourceLines)) {
		reasons.push(`${group.lines.length} lines`);
	}

	const text = group.lang === 'md'
		? group.lines.join(' ')
		: group.lines.map(stripCommentPrefix).join(' ');
	for (const [name, pattern] of TELLS) {
		if (!pattern.test(text)) continue;
		if (name === 'em dash' && DASH_AS_SUBJECT.test(text)) continue;
		reasons.push(name);
	}
	return reasons;
}

/** @param {Fence} fence @param {string} line @returns {Fence} */
function updateFence(fence, line) {
	const match = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
	if (match === null) return fence;
	const run = match[1];
	const character = run[0];
	if (character !== '`' && character !== '~') return fence;
	if (fence === null) return { character, length: run.length };
	if (character === fence.character && run.length >= fence.length && match[2].trim() === '') return null;
	return fence;
}

/** @param {string} source @param {number} line */
function fenceBefore(source, line) {
	/** @type {Fence} */
	let fence = null;
	for (const content of source.split('\n').slice(0, Math.max(0, line - 1))) {
		fence = updateFence(fence, content);
	}
	return fence;
}

/** @param {string} path @param {string} patch @param {string} [source] @returns {Group[]} */
export function groupsFromPatch(path, patch, source) {
	const lang = langFor(path);
	if (lang === null) return [];
	const markdown = lang === 'md';
	const sourceLines = source?.split('\n');
	/** @type {Group[]} */
	const groups = [];
	/** @type {PendingGroup | null} */
	let pending = null;
	let newLine = 0;
	/** @type {Fence} */
	let fence = null;

	/** @param {string} nextLine */
	const flush = nextLine => {
		if (pending !== null) {
			const reasons = reasonsFor(pending, nextLine, sourceLines);
			if (reasons.length > 0) {
				groups.push({
					path,
					start: pending.start,
					end: pending.end,
					text: pending.lines.join('\n'),
					reasons,
				});
			}
		}
		pending = null;
	};

	/** @param {string} content */
	const startsBlock = content => {
		if (!markdown) return isCommentLine(content, lang);
		const nextFence = updateFence(fence, content);
		if (nextFence !== fence) {
			fence = nextFence;
			return false;
		}
		return fence === null && content.trim() !== '' && !/^(?: {4}|\t)/.test(content);
	};

	for (const raw of patch.split('\n')) {
		if (raw.startsWith('@@')) {
			flush('');
			const match = /\+(\d+)/.exec(raw);
			newLine = match === null ? 1 : Number.parseInt(match[1], 10);
			fence = markdown && source !== undefined ? fenceBefore(source, newLine) : null;
			continue;
		}

		if (raw.startsWith('+')) {
			const content = raw.slice(1);
			if (startsBlock(content)) {
				const text = content.trimStart();
				if (pending !== null && (markdown ? MD_ITEM.test(content) : text.startsWith('/**'))) {
					flush(content);
				}
				if (pending === null) {
					pending = {
						start: newLine,
						end: newLine,
						lang,
						topLevel: !/^[\t ]/.test(content),
						doc: text.startsWith('/**'),
						lines: [content],
					};
				} else {
					pending.end = newLine;
					pending.lines.push(content);
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
		if (raw.startsWith('\\')) continue;

		const content = raw.startsWith(' ') ? raw.slice(1) : raw;
		if (markdown) fence = updateFence(fence, content);
		flush(content);
		newLine++;
	}

	flush('');
	return groups;
}

/** @param {Group} group */
export const keyFor = group =>
	createHash('sha256')
		.update(group.path)
		.update('\0')
		.update(String(group.start))
		.update('\0')
		.update(group.text)
		.digest('hex')
		.slice(0, 16);

/** @param {Group} group */
const legacyKeyFor = group => `${group.path}:${createHash('sha256').update(group.text).digest('hex').slice(0, 12)}`;

/** @type {'RIGHT'} */
const RIGHT = 'RIGHT';

/** @param {string} reason */
function guidanceFor(reason) {
	if (/^\d+ lines$/.test(reason)) {
		return 'This is a length-only flag. Check whether each line adds useful context; a necessary explanation can stay.';
	}
	return TELLS.find(([name]) => name === reason)?.[2] ?? 'Review the flagged wording in context.';
}

/** @param {Group} group */
export function bodyFor(group) {
	const guidance = [...new Set(group.reasons.map(guidanceFor))];
	const advice = guidance.length === 1 ? guidance[0] : guidance.map(text => `- ${text}`).join('\n');
	return `<!-- statute-comment-cop:${keyFor(group)} -->\n`
		+ `Flagged for: ${group.reasons.join(', ')}.\n\n${advice}\n\n`
		+ `<sub>Comment Cop is intentionally sensitive; its suggestions are advisory. `
		+ `If this is a false positive, you are welcome to resolve this thread without changing the text.</sub>`
		+ `<br><sub>Any AI agents are only allowed to resolve the comment if it is a false positive and need to justify the closure with a reply. Otherwise fix it. Comment-cop will resolve it automatically on next push.</sub>`;
}

/** @param {unknown} error */
function errorMessage(error) {
	return error instanceof Error ? error.message : String(error);
}

/** @param {unknown} value @param {string} name */
function requiredString(value, name) {
	if (typeof value !== 'string' || value === '') throw new Error(`${name} is required`);
	return value;
}

/** @param {RunArguments} args */
export default async function run({ github, context, core, dryRun = false }) {
	const pullRequest = context.payload.pull_request;
	if (pullRequest === undefined) throw new Error('pull_request payload is required');

	const owner = context.repo.owner;
	const repo = context.repo.repo;
	const pullNumber = pullRequest.number;
	const headSha = requiredString(pullRequest.head.sha, 'pull request head SHA');
	const files = await github.paginate(github.rest.pulls.listFiles, {
		owner,
		repo,
		pull_number: pullNumber,
		per_page: 100,
	});

	/** @type {Group[]} */
	const groups = [];
	const unscannedPaths = new Set();
	for (const file of files) {
		if (file.status === 'removed' || langFor(file.filename) === null) continue;
		if (file.filename.startsWith('vendor/') || file.filename.includes('/vendor/')) continue;
		if (file.patch === undefined) {
			unscannedPaths.add(file.filename);
			core.warning(`No patch available for ${file.filename}; skipping comment scan.`);
			continue;
		}

		let source;
		if (file.filename.endsWith('.go') || langFor(file.filename) === 'md') {
			try {
				const contentsUrl = requiredString(file.contents_url, `contents URL for ${file.filename}`);
				const response = await github.request(contentsUrl, {
					headers: { accept: 'application/vnd.github.raw+json' },
				});
				if (typeof response.data !== 'string') throw new Error(`Invalid contents of ${file.filename}`);
				source = response.data;
			} catch (error) {
				unscannedPaths.add(file.filename);
				core.warning(`Could not read ${file.filename}; skipping comment scan: ${errorMessage(error)}`);
				continue;
			}
		}
		groups.push(...groupsFromPatch(file.filename, file.patch, source));
	}

	const presentKeys = new Set(groups.flatMap(group => [keyFor(group), legacyKeyFor(group)]));
	const seenKeys = new Set();
	/** @type {string[]} */
	const staleThreadIds = [];
	let after = null;
	for (;;) {
		/** @type {ReviewThreadsResponse} */
		const response = await github.graphql(
			`query($owner: String!, $repo: String!, $pr: Int!, $after: String) {
				repository(owner: $owner, name: $repo) {
					pullRequest(number: $pr) {
						reviewThreads(first: 100, after: $after) {
							pageInfo { hasNextPage endCursor }
							nodes { id isResolved path comments(first: 1) { nodes { body viewerDidAuthor } } }
						}
					}
				}
			}`,
			{ owner, repo, pr: pullNumber, after },
		);
		const page = response.repository.pullRequest.reviewThreads;
		for (const thread of page.nodes) {
			const comment = thread.comments.nodes[0];
			if (comment === undefined || !comment.viewerDidAuthor) continue;
			const match = /<!-- statute-comment-cop:(.+) -->/.exec(comment.body);
			if (match === null) continue;
			const key = match[1];
			seenKeys.add(key);
			if (!thread.isResolved && !presentKeys.has(key) && !unscannedPaths.has(thread.path)) {
				staleThreadIds.push(thread.id);
			}
		}
		if (!page.pageInfo.hasNextPage) break;
		after = page.pageInfo.endCursor;
	}

	let resolved = 0;
	for (const id of staleThreadIds) {
		if (dryRun) {
			core.info(`[dry-run] would resolve stale thread ${id}`);
			continue;
		}
		try {
			await github.graphql(
				`mutation($id: ID!) {
					resolveReviewThread(input: {threadId: $id}) { thread { id } }
				}`,
				{ id },
			);
			resolved++;
		} catch (error) {
			core.warning(`Could not resolve Comment Cop thread ${id}: ${errorMessage(error)}`);
		}
	}

	const comments = groups.filter(group => !seenKeys.has(keyFor(group)) && !seenKeys.has(legacyKeyFor(group))).map(
		group => ({
			path: group.path,
			line: group.end,
			side: RIGHT,
			body: bodyFor(group),
			...(group.start < group.end
				? { start_line: group.start, start_side: RIGHT }
				: {}),
		}),
	);
	if (dryRun) {
		for (const group of groups) {
			core.info(`[dry-run] ${group.path}:${group.start}-${group.end} [${group.reasons.join(', ')}]\n${group.text}`);
		}
		core.info(
			comments.length > 0
				? `[dry-run] would submit one review with ${comments.length} new finding(s); nothing posted.`
				: '[dry-run] no new findings; no review to submit.',
		);
		return;
	}
	let posted = 0;
	if (comments.length > 0) {
		try {
			await github.rest.pulls.createReview({
				owner,
				repo,
				pull_number: pullNumber,
				commit_id: headSha,
				event: 'COMMENT',
				body: 'Please review the flagged wording in the inline comments.',
				comments,
			});
			posted = comments.length;
		} catch (error) {
			core.warning(`Could not submit Comment Cop review: ${errorMessage(error)}`);
		}
	}

	core.info(`Comment Cop: ${posted} posted, ${resolved} stale threads resolved, ${groups.length} present.`);
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
		groups.push(...groupsFromPatch(name, git(['diff', '-U3', from, '--', name]), readFileSync(name, 'utf8')));
	}
	for (const name of untracked) {
		groups.push(...groupsFromPatch(name, untrackedPatch(name), readFileSync(name, 'utf8')));
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

/* ------------------------------------------------------------- local entry */

/**
 * Fetch-based client for the scanner's GitHub API operations.
 *
 * @param {string} token
 * @returns {GithubClient}
 */
export function localClient(token) {
	const API = 'https://api.github.com';

	/**
	 * @param {string} path
	 * @param {RequestInit} [init]
	 * @param {boolean} [raw]
	 * @returns {Promise<any>}
	 */
	async function request(path, init, raw = false) {
		const url = new URL(path, API);
		if (url.origin !== API) throw new Error('Refusing non-GitHub API URL');
		const response = await fetch(url, {
			...init,
			redirect: 'error',
			headers: {
				accept: raw ? 'application/vnd.github.raw+json' : 'application/vnd.github+json',
				authorization: `Bearer ${token}`,
				'content-type': 'application/json',
				'x-github-api-version': '2022-11-28',
				'user-agent': 'statute-comment-cop',
			},
		});

		if (!response.ok) {
			throw new Error(`${init?.method ?? 'GET'} ${path} -> ${response.status} ${await response.text()}`);
		}

		return raw ? response.text() : response.json();
	}

	/** @param {Record<string, any>} params */
	const listFiles = params =>
		request(
			`/repos/${params.owner}/${params.repo}/pulls/${params.pull_number}/files`
				+ `?per_page=${params.per_page ?? 100}&page=${params.page ?? 1}`,
		);

	return {
		request: async url => ({ data: await request(url, undefined, true) }),
		rest: {
			pulls: {
				listFiles,
				createReview: params =>
					request(`/repos/${params.owner}/${params.repo}/pulls/${params.pull_number}/reviews`, {
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

const USAGE = `comment-cop — review added comments and Markdown prose.

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
