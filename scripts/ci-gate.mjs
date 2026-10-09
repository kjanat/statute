// @ts-check
import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';

export const conditionalJobs = ['rewrite', 'binaryen', 'labels'];

/** @param {string[]} paths */
export function selectChecks(paths) {
	const gate = paths.some(path => path === '.github/workflows/ci.yml' || path.startsWith('scripts/ci-gate'));
	const research = gate
		|| paths.some(path =>
			['.mise.toml', 'mise.lock', 'package.json', 'package-lock.json', 'go.mod', 'go.sum'].includes(path)
			|| /^[^/]+\.go$/.test(path) || /^(internal|resolved|research\/htmlrewrite)\//.test(path)
		);
	return {
		rewrite: research || paths.includes('.github/workflows/research-htmlrewrite.yml'),
		binaryen: research || paths.includes('.github/workflows/research-htmlrewrite-binaryen.yml'),
		labels: gate
			|| paths.some(path => path === '.github/workflows/labels.yml' || /^\.github\/(?:.*\/)?labels\.yml$/.test(path)),
	};
}

/** @typedef {{result: string, outputs?: Record<string, string>}} Job */
/** @param {Record<string, Job>} needs */
export function validateGate(needs) {
	if (!needs.select_checks) throw new Error('Missing check selection');
	for (const [name, job] of Object.entries(needs)) {
		if (!conditionalJobs.includes(name) && job.result !== 'success') throw new Error(`${name}: ${job.result}`);
	}
	const outputs = needs.select_checks.outputs ?? {};
	for (const name of conditionalJobs) {
		const selected = outputs[name];
		if (selected !== 'true' && selected !== 'false') throw new Error(`${name}: missing Boolean selection`);
		const required = selected === 'true' ? 'success' : 'skipped';
		if (needs[name]?.result !== required) throw new Error(`${name}: ${needs[name]?.result}; required ${required}`);
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
