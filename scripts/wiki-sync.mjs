#!/usr/bin/env node
// @ts-check
import { mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { env } from 'node:process';

const REPO = !!env.GITHUB_ACTIONS
	? `${env.GITHUB_SERVER_URL}/${env.GITHUB_REPOSITORY}`
	: 'https://github.com/kjanat/statute';

/**
 * Page name for a document, from its title. A wiki page is addressed by its
 * file name, so the name carries nothing that would have to be escaped.
 *
 * @param {string} body
 * @param {string} source
 * @returns {string}
 */
function title(body, source) {
	const first = /^#[ \t]+(.+?)[ \t]*$/m.exec(body.split('\n\n', 1)[0] ?? '');
	if (first?.[1] === undefined) throw new Error(`docs/${source}: no title on the first line`);
	const page = first[1].replace(/[^A-Za-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
	if (page === '') throw new Error(`docs/${source}: title has no letters or digits`);
	return page;
}

/**
 * @param {string} body
 * @param {string} source
 * @param {ReadonlyMap<string, string>} pages
 * @returns {string}
 */
function render(body, source, pages) {
	return body.replace(/^#[ \t]+.+?\n\n/, '').replace(
		/\]\(([^)\s]+)\)/g,
		/** @type {(match: string, target: string) => string} */ (match, target) => {
			if (/^(?:[a-z][a-z0-9+.-]*:|\/\/|#)/i.test(target)) return match;
			const [path, anchor] = splitAnchor(target);
			if (path.startsWith('../')) {
				const kind = path.endsWith('/') ? 'tree' : 'blob';
				return `](${REPO}/${kind}/HEAD/${path.slice(3)}${anchor})`;
			}
			const page = pages.get(path);
			if (page === undefined) {
				throw new Error(`docs/${source}: link to ${path}, which is not a synced document`);
			}
			return `](./${page}${anchor})`;
		},
	);
}

/**
 * @param {string} target
 * @returns {[string, string]}
 */
function splitAnchor(target) {
	const hash = target.indexOf('#');
	return hash === -1 ? [target, ''] : [target.slice(0, hash), target.slice(hash)];
}

const [docsDir, wikiDir] = process.argv.slice(2);
if (docsDir === undefined || wikiDir === undefined) {
	throw new Error('usage: node scripts/wiki-sync.mjs <docs-dir> <wiki-dir>');
}

const sources = (await readdir(docsDir)).filter((f) => f.endsWith('.md')).sort();
if (sources.length === 0) throw new Error(`${docsDir} holds no documents`);

/** @type {Map<string, string>} */
const bodies = new Map();
/** @type {Map<string, string>} */
const pages = new Map();
/** @type {Map<string, string>} */
const claimed = new Map();

for (const source of sources) {
	const body = await readFile(join(docsDir, source), 'utf8');
	const page = title(body, source);
	const other = claimed.get(page);
	if (other !== undefined) {
		throw new Error(`docs/${source} and docs/${other} both claim the page name ${page}`);
	}
	claimed.set(page, source);
	bodies.set(source, body);
	pages.set(source, page);
}

const target = join(wikiDir, 'Docs');
await rm(target, { force: true, recursive: true });
await mkdir(target, { recursive: true });

for (const source of sources) {
	const page = pages.get(source);
	const body = bodies.get(source);
	if (page === undefined || body === undefined) throw new Error(`docs/${source} went missing`);
	await writeFile(join(target, `${page}.md`), render(body, source, pages));
	console.log(`docs/${source} -> Docs/${page}.md`);
}
