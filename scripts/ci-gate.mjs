// @ts-check
import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';

export const ordinaryJobs = [
	'test',
	'lint',
	'lifecycle_lint',
	'discover_fuzz',
	'fuzz',
	'examples',
	'apidiff',
	'actionlint',
	'typecheck',
	'e2e',
	'select_checks',
];
export const conditionalJobs = ['rewrite', 'binaryen', 'labels'];

/** @param {string[]} paths */
export function selectChecks(paths) {
	const gate = paths.some(path => path === '.github/workflows/ci.yml' || path.startsWith('scripts/ci-gate'));
	return {
		rewrite: gate
			|| paths.some(path =>
				/^[^/]+\.go$/.test(path) || ['go.mod', 'go.sum', '.github/workflows/research-htmlrewrite.yml'].includes(path)
				|| /^(internal|resolved|research\/htmlrewrite)\//.test(path)
			),
		binaryen: gate
			|| paths.some(path =>
				path.startsWith('research/htmlrewrite/') || path === '.github/workflows/research-htmlrewrite-binaryen.yml'
			),
		labels: gate
			|| paths.some(path => path === '.github/workflows/labels.yml' || /^\.github\/(?:.*\/)?labels\.yml$/.test(path)),
	};
}

/** @typedef {{result: string, outputs?: Record<string, string>}} Job */
/** @param {Record<string, Job>} needs */
export function validateGate(needs) {
	const expected = [...ordinaryJobs, ...conditionalJobs];
	if (Object.keys(needs).length !== expected.length || expected.some(name => !needs[name])) {
		throw new Error('CI gate dependency set is incomplete or unexpected');
	}
	for (const name of ordinaryJobs) {
		if (needs[name].result !== 'success') throw new Error(`${name}: ${needs[name].result}`);
	}
	const outputs = needs.select_checks.outputs ?? {};
	for (const name of conditionalJobs) {
		const selected = outputs[name];
		if (selected !== 'true' && selected !== 'false') throw new Error(`${name}: missing Boolean selection`);
		const required = selected === 'true' ? 'success' : 'skipped';
		if (needs[name].result !== required) throw new Error(`${name}: ${needs[name].result}; required ${required}`);
	}
}

if (import.meta.main) {
	if (process.argv[2] === 'select') {
		const { BASE_SHA: base = '', HEAD_SHA: head = '', GITHUB_OUTPUT: output } = process.env;
		if (!/^[a-f0-9]{40}$/.test(base) || !/^[a-f0-9]{40}$/.test(head) || !output) {
			throw new Error('Missing exact PR commits or output path');
		}
		const changed = execFileSync('git', ['diff', '--name-only', '--no-renames', '-z', `${base}...${head}`], {
			encoding: 'utf8',
			maxBuffer: 16 << 20,
		});
		const selection = selectChecks(changed.split('\0').filter(Boolean));
		appendFileSync(output, Object.entries(selection).map(([name, selected]) => `${name}=${selected}\n`).join(''));
		console.log(JSON.stringify(selection));
	} else if (process.argv[2] === 'validate') {
		validateGate(JSON.parse(process.env.NEEDS_JSON ?? '{}'));
		console.log('All selected CI jobs passed.');
	} else {
		throw new Error('usage: ci-gate.mjs select|validate');
	}
}
