#!/usr/bin/env node
// @ts-check
// Builds a self-contained API reference at ${OUT}/api/index.html from the
// pkg.go.dev v1 API (public, GET only, no auth). Run: node scripts/api-page.mjs
//
// The page is static: inline CSS, no external requests, no fonts, no analytics.
// The only JavaScript is an optional client-side filter box that stays hidden
// unless scripting is available, so the page is complete without it.

import { mkdir, writeFile } from 'node:fs/promises';

const HOST = process.env.HOST || 'statute.kjanat.dev';
const OUT = process.env.OUT || 'dist';
const REPO = process.env.REPO || '';
const BRANCH = process.env.BRANCH || 'master';

const API = 'https://pkg.go.dev/v1';
const GODEV = `https://pkg.go.dev/${HOST}`;

/* ------------------------------------------------------------------- types */

// The pkg.go.dev v1 responses, narrowed to the fields this page reads. Fields
// the API always sends are required; the rest mirror the defensive reads below.

/** @typedef {'Type'|'Function'|'Method'|'Field'|'Constant'|'Variable'} Kind */

/**
 * One exported symbol. `parent` is the type that owns it, or the symbol's own
 * name when it is package-level.
 * @typedef {object} Sym
 * @property {string} name
 * @property {Kind} kind
 * @property {string} synopsis
 * @property {string} parent
 */

/**
 * A page of symbols. Everything is optional: the paging loop treats a missing
 * bag as "nothing more to read" rather than an error.
 * @typedef {object} SymbolBag
 * @property {Sym[]} [items]
 * @property {number} [total]
 * @property {string} [nextPageToken]
 */

/**
 * GET /v1/symbols/{path}
 * @typedef {object} SymbolsResponse
 * @property {string} modulePath
 * @property {string} version
 * @property {SymbolBag} [symbols]
 * @property {string} [nextPageToken]
 */

/**
 * GET /v1/module/{path}
 * @typedef {object} ModuleResponse
 * @property {string} path
 * @property {string} version
 * @property {string} commitTime
 * @property {boolean} isLatest
 * @property {boolean} isRedistributable
 * @property {boolean} hasGoMod
 * @property {string} [repoUrl]
 */

/**
 * One entry of GET /v1/packages/{path}
 * @typedef {object} PackageItem
 * @property {string} path
 * @property {string} name
 * @property {string} [synopsis]
 * @property {boolean} [isRedistributable]
 */

/**
 * GET /v1/packages/{path}
 * @typedef {object} PackagesResponse
 * @property {string} modulePath
 * @property {string} version
 * @property {{items?: PackageItem[]}} [packages]
 */

/**
 * An Error carrying the "do not retry" flag getJSON stamps on hard failures.
 * @typedef {Error & {fatal?: boolean}} FetchError
 */

// What this script derives from the above and hands to the renderers.

/**
 * A type declaration plus every symbol pkg.go.dev hangs off it.
 * @typedef {object} TypeGroup
 * @property {string} name
 * @property {Sym} decl
 * @property {Sym[]} members
 */

/**
 * A package's symbols split into the three things the page renders.
 * @typedef {object} Groups
 * @property {TypeGroup[]} types
 * @property {Sym[]} funcs
 * @property {Sym[]} values
 */

/**
 * A package as the renderers see it: API fields plus slug and grouping.
 * @typedef {object} Pkg
 * @property {string} path
 * @property {string} name
 * @property {string} synopsis
 * @property {string} slug
 * @property {Sym[]} symbols
 * @property {Groups} groups
 */

/* ---------------------------------------------------------------- fetching */

/** @type {(ms: number) => Promise<void>} */
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * GET JSON with a small exponential backoff; 4xx (except 429) fails fast.
 * @template T the expected response shape; the caller declares it.
 * @param {string} url
 * @param {number} [attempt]
 * @returns {Promise<T>}
 */
async function getJSON(url, attempt = 0) {
	try {
		const res = await fetch(url, { headers: { accept: 'application/json' } });
		if (!res.ok) {
			const err = /** @type {FetchError} */ (new Error(`HTTP ${res.status} ${res.statusText}`));
			err.fatal = res.status >= 400 && res.status < 500 && res.status !== 429;
			throw err;
		}
		return await res.json();
	} catch (thrown) {
		const err = /** @type {FetchError} */ (thrown);
		if (err.fatal || attempt >= 3) throw Object.assign(new Error(`GET ${url}: ${err.message}`), { cause: err });
		await sleep(500 * 2 ** attempt);
		return getJSON(url, attempt + 1);
	}
}

/**
 * All symbols of a package, following nextPageToken.
 * @param {string} pkgPath
 * @returns {Promise<Sym[]>}
 */
async function fetchSymbols(pkgPath) {
	/** @type {Sym[]} */
	const items = [];
	/** @type {Set<string>} */
	const seen = new Set();
	let token = '';
	for (let page = 0; page < 50; page++) {
		const url = `${API}/symbols/${pkgPath}${token ? `?token=${encodeURIComponent(token)}` : ''}`;
		/** @type {SymbolsResponse} */
		const data = await getJSON(url);
		/** @type {SymbolBag} */
		const bag = data.symbols || {};
		items.push(...(bag.items || []));
		token = bag.nextPageToken || data.nextPageToken || '';
		if (!token || seen.has(token)) break;
		seen.add(token);
	}
	return items;
}

/* ---------------------------------------------------------------- grouping */

// Order members inside a type card: declaration, constructors, methods,
// then the data surface.
/** @type {Record<Kind, number>} */
const KIND_RANK = { Type: 0, Function: 1, Method: 2, Constant: 3, Variable: 4, Field: 5 };
/** @type {[Kind, string][]} */
const MEMBER_GROUPS = [
	['Function', 'Functions'],
	['Method', 'Methods'],
	['Constant', 'Constants'],
	['Variable', 'Variables'],
	['Field', 'Fields'],
];

/** @type {(a: {name: string}, b: {name: string}) => number} */
const byName = (a, b) => a.name.localeCompare(b.name, 'en');

/**
 * pkg.go.dev groups every symbol under the type it belongs to via `parent`;
 * package-level funcs/vars are their own parent. Split those two cases.
 * @param {Sym[]} items
 * @returns {Groups}
 */
function group(items) {
	/** @type {Map<string, Sym[]>} */
	const buckets = new Map();
	for (const sym of items) {
		const key = sym.parent || sym.name;
		if (!buckets.has(key)) buckets.set(key, []);
		/** @type {Sym[]} */ (buckets.get(key)).push(sym);
	}

	/** @type {TypeGroup[]} */
	const types = [];
	/** @type {Sym[]} */
	const funcs = [];
	/** @type {Sym[]} */
	const values = [];
	for (const [name, syms] of buckets) {
		syms.sort((a, b) => (KIND_RANK[a.kind] ?? 9) - (KIND_RANK[b.kind] ?? 9) || byName(a, b));
		const decl = syms.find((s) => s.kind === 'Type');
		if (decl) {
			types.push({ name, decl, members: syms.filter((s) => s !== decl) });
			continue;
		}
		for (const sym of syms) (sym.kind === 'Function' ? funcs : values).push(sym);
	}
	types.sort(byName);
	funcs.sort(byName);
	values.sort(byName);
	return { types, funcs, values };
}

/* --------------------------------------------------------------- rendering */

/** @type {Record<string, string>} */
const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
/** Escape an API-sourced string for text or attribute context.
 * @type {(s: unknown) => string} */
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESCAPES[c]);

/** Stable, readable element id prefix for a package.
 * @type {(s: string) => string} */
const slugify = (s) => s.replace(/[^A-Za-z0-9]+/g, '-').replace(/^-|-$/g, '');

/** Lowercased haystack for the filter box.
 * @type {(...parts: string[]) => string} */
const query = (...parts) => esc(parts.join(' ').toLowerCase());

/** Members list their own name relative to the owning type.
 * @type {(name: string, parent: string) => string} */
const shortName = (name, parent) => (name.startsWith(`${parent}.`) ? name.slice(parent.length + 1) : name);

/**
 * @param {Sym} sym
 * @param {Pkg} pkg
 * @param {string} parent owning type name, or '' for a package-level list
 * @returns {string}
 */
function renderMember(sym, pkg, parent) {
	const href = `https://pkg.go.dev/${pkg.path}#${encodeURIComponent(sym.name)}`;
	return `<li class="row" data-q="${query(sym.name, sym.synopsis)}">`
		+ `<a class="name" href="${esc(href)}">${esc(shortName(sym.name, parent))}</a>`
		+ `<code>${esc(sym.synopsis)}</code></li>`;
}

/**
 * @param {TypeGroup} type
 * @param {Pkg} pkg
 * @returns {string}
 */
function renderType(type, pkg) {
	const id = `${pkg.slug}.${type.name}`;
	const href = `https://pkg.go.dev/${pkg.path}#${encodeURIComponent(type.name)}`;
	const haystack = [type.name, type.decl.synopsis, ...type.members.map((m) => `${m.name} ${m.synopsis}`)];

	const sections = [];
	for (const [kind, label] of MEMBER_GROUPS) {
		const members = type.members.filter((m) => m.kind === kind);
		if (members.length === 0) continue;
		sections.push(
			`<h4>${label}</h4><ul>${members.map((m) => renderMember(m, pkg, type.name)).join('')}</ul>`,
		);
	}

	return `<article class="sym" id="${esc(id)}" data-q="${query(...haystack)}">
<h3><a class="name" href="${esc(href)}">${esc(type.name)}</a></h3>
<code class="decl">${esc(type.decl.synopsis)}</code>
${sections.join('\n')}
</article>`;
}

/**
 * @param {string} title
 * @param {Sym[]} syms
 * @param {Pkg} pkg
 * @returns {string}
 */
function renderList(title, syms, pkg) {
	if (syms.length === 0) return '';
	const rows = syms.map((s) => renderMember(s, pkg, '')).join('');
	return `<article class="sym flat" id="${esc(`${pkg.slug}.--${slugify(title)}`)}" data-q="${
		query(title, ...syms.map((s) => `${s.name} ${s.synopsis}`))
	}">
<h3>${esc(title)}</h3>
<ul>${rows}</ul>
</article>`;
}

/**
 * @param {Pkg} pkg
 * @returns {string}
 */
function renderPackage(pkg) {
	const { types, funcs, values } = pkg.groups;
	const chips = types
		.map((t) => `<a href="#${esc(`${pkg.slug}.${t.name}`)}" data-q="${query(t.name)}">${esc(t.name)}</a>`)
		.join('');

	return `<section class="pkg" id="${esc(pkg.slug)}">
<header class="pkg-head">
<h2><a class="name" href="https://pkg.go.dev/${esc(pkg.path)}">${esc(pkg.path)}</a></h2>
<p class="synopsis">${esc(pkg.synopsis)}</p>
<p class="counts">${pkg.symbols.length} symbols &middot; ${types.length} types &middot; ${funcs.length} functions</p>
${chips ? `<nav class="chips">${chips}</nav>` : ''}
</header>
${renderList('Functions', funcs, pkg)}
${renderList('Variables', values, pkg)}
<div class="grid">
${types.map((t) => renderType(t, pkg)).join('\n')}
</div>
</section>`;
}

const CSS = `
:root {
	color-scheme: light dark;
	--bg: #fbfbf9;
	--panel: #fff;
	--fg: #1b1c1a;
	--muted: #6c6d68;
	--line: #e2e2dc;
	--accent: #1b52c0;
	--code: #f4f4f0;
}
@media (prefers-color-scheme: dark) {
	:root {
		--bg: #101114;
		--panel: #17181c;
		--fg: #e4e5e1;
		--muted: #93948d;
		--line: #292b30;
		--accent: #8fb0ff;
		--code: #1d1f24;
	}
}
* { box-sizing: border-box; }
body {
	margin: 0;
	padding: 2.5rem 1.25rem 4rem;
	background: var(--bg);
	color: var(--fg);
	font: 400 14px/1.55 ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
	-webkit-text-size-adjust: 100%;
}
.wrap { max-width: 78rem; margin: 0 auto; }
a { color: var(--accent); text-decoration: none; }
a:hover { text-decoration: underline; }
code { font-family: inherit; }
p { margin: 0.35rem 0; }
h1, h2, h3, h4 { margin: 0; font-weight: 600; letter-spacing: -0.01em; }

.masthead { border-bottom: 1px solid var(--line); padding-bottom: 1.25rem; margin-bottom: 2rem; }
.masthead h1 { font-size: 1.5rem; }
.tag {
	display: inline-block;
	margin-left: 0.5rem;
	padding: 0.05rem 0.4rem;
	border: 1px solid var(--line);
	border-radius: 0.3rem;
	background: var(--code);
	color: var(--muted);
	font-size: 0.8rem;
	vertical-align: 0.15em;
}
.lede { max-width: 60ch; margin-top: 0.6rem; color: var(--muted); font-family: system-ui, -apple-system, Segoe UI, sans-serif; }
.links { margin-top: 0.8rem; display: flex; flex-wrap: wrap; gap: 0.85rem; }

.filter { display: none; margin-top: 1rem; gap: 0.5rem; align-items: center; }
.filter input {
	flex: 1 1 auto;
	min-width: 0;
	max-width: 26rem;
	padding: 0.35rem 0.6rem;
	border: 1px solid var(--line);
	border-radius: 0.35rem;
	background: var(--panel);
	color: inherit;
	font: inherit;
}
.filter input:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
.filter output { color: var(--muted); font-size: 0.85rem; }

.pkg { margin-bottom: 3rem; }
.pkg-head { margin-bottom: 1rem; }
.pkg-head h2 { font-size: 1.05rem; }
.synopsis { max-width: 72ch; color: var(--fg); font-family: system-ui, -apple-system, Segoe UI, sans-serif; }
.counts { color: var(--muted); font-size: 0.85rem; }
.chips { display: flex; flex-wrap: wrap; gap: 0.3rem; margin-top: 0.7rem; }
.chips a {
	padding: 0.05rem 0.4rem;
	border: 1px solid var(--line);
	border-radius: 0.3rem;
	background: var(--panel);
	color: var(--muted);
	font-size: 0.8rem;
}
.chips a:hover { color: var(--accent); border-color: var(--accent); text-decoration: none; }

/* Columns rather than grid: type cards differ wildly in height and masonry
   packing keeps the page dense without leaving row-aligned gaps. */
.grid { columns: 26rem auto; column-gap: 0.85rem; }
.sym {
	padding: 0.85rem 0.95rem;
	border: 1px solid var(--line);
	border-radius: 0.5rem;
	background: var(--panel);
	scroll-margin-top: 1rem;
}
.grid .sym {
	display: inline-block;
	width: 100%;
	margin-bottom: 0.85rem;
	break-inside: avoid;
}
.sym.flat { margin-bottom: 0.85rem; }
.sym.flat ul { columns: 24rem auto; column-gap: 1.6rem; }
.sym.flat .row { break-inside: avoid; }
.sym h3 { font-size: 0.95rem; }
.sym h4 {
	margin: 0.85rem 0 0.3rem;
	color: var(--muted);
	font-size: 0.72rem;
	font-weight: 600;
	letter-spacing: 0.08em;
	text-transform: uppercase;
}
.decl {
	display: block;
	margin-top: 0.35rem;
	padding: 0.3rem 0.45rem;
	border-radius: 0.3rem;
	background: var(--code);
	color: var(--muted);
	font-size: 0.85rem;
	overflow-x: auto;
}
.sym ul { margin: 0; padding: 0; list-style: none; }
.sym.flat ul { margin-top: 0.5rem; }
.row { padding: 0.28rem 0; border-top: 1px solid var(--line); }
.row:first-child { border-top: 0; }
.row .name { display: inline-block; font-weight: 600; }
.row code { display: block; color: var(--muted); font-size: 0.85rem; overflow-x: auto; }

footer {
	margin-top: 3rem;
	padding-top: 1.25rem;
	border-top: 1px solid var(--line);
	color: var(--muted);
	font-family: system-ui, -apple-system, Segoe UI, sans-serif;
	font-size: 0.85rem;
}
[hidden] { display: none !important; }
`;

// Progressive enhancement only: the filter bar is revealed by this script and
// stays absent when scripting is off.
const JS = `
(function () {
	var bar = document.querySelector('.filter');
	var box = document.getElementById('q');
	var out = document.getElementById('n');
	if (!bar || !box) return;
	bar.style.display = 'flex';
	var nodes = [].slice.call(document.querySelectorAll('[data-q]'));
	var sections = [].slice.call(document.querySelectorAll('.pkg'));
	function apply() {
		var q = box.value.trim().toLowerCase();
		var shown = 0;
		for (var i = 0; i < nodes.length; i++) {
			var hit = !q || nodes[i].dataset.q.indexOf(q) !== -1;
			nodes[i].hidden = !hit;
			if (hit && nodes[i].classList.contains('sym')) shown++;
		}
		for (var j = 0; j < sections.length; j++) {
			sections[j].hidden = !!q && !sections[j].querySelector('.sym:not([hidden])');
		}
		if (out) out.textContent = q ? shown + ' matching' : '';
	}
	box.addEventListener('input', apply);
	apply();
})();
`;

/**
 * @param {{module: ModuleResponse, packages: Pkg[], repo: string, total: number}} data
 * @returns {string}
 */
function renderPage({ module: mod, packages, repo, total }) {
	const goImport = `${HOST} git ${repo}`;
	const goSource = `${HOST} ${repo} ${repo}/tree/${BRANCH}{/dir} ${repo}/blob/${BRANCH}{/dir}/{file}#L{line}`;
	const stamp = mod.commitTime ? String(mod.commitTime).slice(0, 10) : '';

	return `<!DOCTYPE html>
<html lang="en">
	<head>
		<meta charset="utf-8">
		<meta name="viewport" content="width=device-width, initial-scale=1">
		<title>${esc(HOST)} API reference</title>
		<meta name="description" content="Exported API surface of the ${
		esc(HOST)
	} Go module, generated from the pkg.go.dev API.">
		<meta name="go-import" content="${esc(goImport)}">
		<meta name="go-source" content="${esc(goSource)}">
		<style>${CSS}</style>
	</head>
	<body>
		<div class="wrap">
			<header class="masthead">
				<h1>${esc(HOST)}<span class="tag">${esc(mod.version)}</span></h1>
				<p class="lede">${esc(packages[0]?.synopsis || '')}</p>
				<nav class="links">
					<a href="${esc(GODEV)}">pkg.go.dev</a>
					<a href="${esc(repo)}">source</a>
					<a href="${esc(`${repo}/releases/tag/${mod.version}`)}">release ${esc(mod.version)}</a>
					<a href="/">module root</a>
				</nav>
				<div class="filter">
					<label for="q">filter</label>
					<input id="q" type="search" placeholder="type, func, field&hellip;" autocomplete="off" spellcheck="false">
					<output id="n" for="q"></output>
				</div>
			</header>
			<main>
${packages.map(renderPackage).join('\n')}
			</main>
			<footer>
				<p>Generated from the <a href="https://pkg.go.dev/${
		esc(HOST)
	}">pkg.go.dev</a> v1 API &mdash; ${total} exported symbols across ${packages.length} package${
		packages.length === 1 ? '' : 's'
	} at module version <code>${esc(mod.version)}</code>${stamp ? ` (${esc(stamp)})` : ''}.</p>
				<p>Documentation comments live in the source; this page is a reference card of the surface.</p>
			</footer>
		</div>
		<script>${JS}</script>
	</body>
</html>
`;
}

/* -------------------------------------------------------------------- main */

/** examples/* are package main and internal/* is not importable: skip both.
 * @type {(pkg: PackageItem) => boolean} */
const isPublicLibrary = (pkg) => pkg.name !== 'main' && !`${pkg.path}/`.slice(HOST.length).includes('/internal/');

/** @returns {Promise<void>} */
async function main() {
	/** @type {[ModuleResponse, PackagesResponse]} */
	const [mod, pkgList] = await Promise.all([
		getJSON(`${API}/module/${HOST}`),
		getJSON(`${API}/packages/${HOST}`),
	]);

	const repo = REPO || mod.repoUrl || `https://github.com/kjanat/${HOST.split('.')[0]}`;
	const candidates = (pkgList.packages?.items || []).filter(isPublicLibrary);
	if (candidates.length === 0) throw new Error(`no library packages found in ${HOST}`);

	/** @type {Pkg[]} */
	const packages = [];
	for (const pkg of candidates) {
		const symbols = await fetchSymbols(pkg.path);
		if (symbols.length === 0) continue;
		packages.push({
			path: pkg.path,
			name: pkg.name,
			synopsis: pkg.synopsis || '',
			slug: pkg.path === HOST ? pkg.name : slugify(pkg.path.slice(HOST.length)),
			symbols,
			groups: group(symbols),
		});
	}

	const total = packages.reduce((n, p) => n + p.symbols.length, 0);
	const html = renderPage({ module: mod, packages, repo, total });

	await mkdir(`${OUT}/api`, { recursive: true });
	await writeFile(`${OUT}/api/index.html`, html);
	console.log(
		`wrote ${OUT}/api/index.html: ${total} symbols, ${packages.length} packages, ${mod.version}, ${html.length} bytes`,
	);
}

await main();
