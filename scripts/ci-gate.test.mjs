import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import {
	existsSync,
	mkdirSync,
	mkdtempSync,
	readdirSync,
	readFileSync,
	renameSync,
	rmSync,
	writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { parse } from 'yaml';

import { conditionalJobs, ordinaryJobs, selectChecks, validateGate } from './ci-gate.mjs';

/** @param {boolean} selected */
function results(selected) {
	/** @type {Record<string, {result: string, outputs?: Record<string, string>}>} */
	const needs = Object.fromEntries(ordinaryJobs.map(name => [name, { result: 'success' }]));
	needs.select_checks.outputs = Object.fromEntries(conditionalJobs.map(name => [name, String(selected)]));
	for (const name of conditionalJobs) needs[name] = { result: selected ? 'success' : 'skipped' };
	return needs;
}

test('docs skip expensive checks; selected calls must pass completely', () => {
	assert.deepEqual(selectChecks(['docs/production.md']), { rewrite: false, binaryen: false, labels: false });
	assert.doesNotThrow(() => validateGate(results(false)));
	assert.doesNotThrow(() => validateGate(results(true)));
	for (const name of [...ordinaryJobs, ...conditionalJobs]) {
		for (const state of ['failure', 'cancelled', 'timed_out', 'skipped', 'queued', 'in_progress', 'neutral']) {
			const needs = results(true);
			needs[name].result = state;
			assert.throws(() => validateGate(needs), { message: new RegExp(name) });
		}
	}
});

test('selector errors and unknown dependencies fail closed', () => {
	for (const value of ['', 'True', '0', 'yes']) {
		const needs = results(false);
		needs.select_checks.outputs = { rewrite: value, binaryen: 'false', labels: 'false' };
		assert.throws(() => validateGate(needs), /Boolean/);
	}
	const missing = results(true);
	delete missing.fuzz;
	assert.throws(() => validateGate(missing), /dependency set/);
	assert.throws(() => validateGate({ ...results(true), future_job: { result: 'failure' } }), /dependency set/);
	const unexpectedlySkipped = results(true);
	unexpectedlySkipped.binaryen.result = 'skipped';
	assert.throws(() => validateGate(unexpectedlySkipped), /binaryen/);
});

test('path selection preserves lane boundaries and includes gate changes', () => {
	for (
		const path of [
			'server.go',
			'go.mod',
			'go.sum',
			'internal/probe/a.go',
			'resolved/resolved.go',
			'.github/workflows/research-htmlrewrite.yml',
		]
	) {
		assert.deepEqual(selectChecks([path]), { rewrite: true, binaryen: false, labels: false });
	}
	assert.deepEqual(selectChecks(['research/htmlrewrite/http_test.go']), {
		rewrite: true,
		binaryen: true,
		labels: false,
	});
	assert.deepEqual(selectChecks(['.github/workflows/research-htmlrewrite-binaryen.yml']), {
		rewrite: false,
		binaryen: true,
		labels: false,
	});
	for (const path of ['.github/labels.yml', '.github/nested/labels.yml', '.github/workflows/labels.yml']) {
		assert.deepEqual(selectChecks([path]), { rewrite: false, binaryen: false, labels: true });
	}
	for (const path of ['.github/workflows/ci.yml', 'scripts/ci-gate.mjs', 'scripts/ci-gate.test.mjs']) {
		assert.deepEqual(selectChecks([path]), { rewrite: true, binaryen: true, labels: true });
	}
	assert.deepEqual(selectChecks(['docs/server.go', 'docs/research/htmlrewrite.md']), {
		rewrite: false,
		binaryen: false,
		labels: false,
	});
});

/** @param {string} name */
function workflow(name) {
	return parse(readFileSync(new URL(`../.github/workflows/${name}.yml`, import.meta.url), 'utf8'));
}

test('gate needs every CI job, including complete dynamic and reusable matrices', () => {
	const ci = workflow('ci');
	assert.deepEqual(new Set(ci.jobs.gate.needs), new Set(Object.keys(ci.jobs).filter(name => name !== 'gate')));
	assert.deepEqual(new Set(ci.jobs.gate.needs), new Set([...ordinaryJobs, ...conditionalJobs]));
	assert.equal(ci.jobs.gate.if, "always() && github.event_name == 'pull_request'");
	assert.equal(ci.jobs.gate.name, 'ci gate');
	for (const [name, file] of [['rewrite', 'research-htmlrewrite'], ['binaryen', 'research-htmlrewrite-binaryen']]) {
		assert.equal(ci.jobs[name].uses, `./.github/workflows/${file}.yml`);
		assert.equal(ci.jobs[name].if, `needs.select_checks.outputs.${name} == 'true'`);
		const reusable = workflow(file);
		assert.ok(Object.hasOwn(reusable.on, 'workflow_call'));
		assert.ok(!Object.hasOwn(reusable.on, 'pull_request'));
		assert.deepEqual(reusable.permissions, { contents: 'read' });
		assert.notEqual(reusable.concurrency.group, ci.concurrency.group);
	}
});

test('format and lint are independent with setup-go owning the compiler', () => {
	const { format, lint } = workflow('ci').jobs;
	for (const job of [format, lint]) {
		assert.equal(job.needs, undefined);
		assert.equal(job.steps.filter(step => step.uses?.startsWith('actions/setup-go@')).length, 1);
	}
	assert.equal(format.env.MISE_DISABLE_TOOLS, 'go');
	const mise = format.steps.find(step => step.uses?.startsWith('jdx/mise-action@'));
	assert.equal(mise.with.install_args, 'dprint tombi');
	assert.equal(mise.with.add_shims_to_path, false);
	assert.ok(format.steps.some(step => step.run === 'make fmt-check'));
	assert.ok(!lint.steps.some(step => step.uses?.startsWith('jdx/mise-action@') || step.run === 'make fmt-check'));
	assert.ok(lint.steps.some(step => step.uses?.startsWith('golangci/golangci-lint-action@')));
});

test('PR label validation is read-only; publication keeps its push trigger', () => {
	const ci = workflow('ci');
	assert.deepEqual(ci.jobs.labels.permissions, { contents: 'read', issues: 'read' });
	assert.equal(ci.jobs.labels.steps.at(-1).with['dry-run'], 'true');
	const labels = workflow('labels');
	assert.ok(!Object.hasOwn(labels.on, 'pull_request'));
	assert.ok(Object.hasOwn(labels.on, 'push'));
});

test('standalone PR workflows have complete registered gates', () => {
	const files = readdirSync(new URL('../.github/workflows/', import.meta.url)).filter(name => /\.ya?ml$/.test(name));
	const producers = files.filter(file => {
		const triggers = workflow(file.replace(/\.ya?ml$/, '')).on;
		const events = typeof triggers === 'string'
			? [triggers]
			: Array.isArray(triggers)
			? triggers
			: Object.keys(triggers ?? {});
		return events.includes('pull_request') || events.includes('pull_request_target');
	});
	assert.deepEqual(producers.sort(), ['ci.yml', 'codeql.yml', 'comment-cop.yml']);
	const codeql = workflow('codeql');
	assert.deepEqual(new Set(codeql.jobs.gate.needs), new Set(Object.keys(codeql.jobs).filter(name => name !== 'gate')));
	assert.equal(codeql.jobs.gate.name, 'CodeQL gate');
	assert.equal(codeql.jobs.gate.if, 'always()');
	assert.equal(codeql.jobs.gate.steps[0].run, 'test "${RESULT}" = success');
	assert.equal(codeql.jobs.gate.steps[0].env.RESULT, '${{ needs.analyze.result }}');
	assert.deepEqual(Object.keys(workflow('comment-cop').jobs), ['comment-cop']);
});

test('CLI discovers deleted and newline paths; unavailable commits cannot select false', t => {
	const dir = mkdtempSync(join(tmpdir(), 'statute-ci-gate-'));
	t.after(() => rmSync(dir, { recursive: true, force: true }));
	/** @param {...string} args */
	const git = (...args) => execFileSync('git', args, { cwd: dir, encoding: 'utf8' }).trim();
	git('init', '-q');
	git('config', 'user.name', 'CI gate test');
	git('config', 'user.email', 'ci-gate@example.invalid');
	mkdirSync(join(dir, 'research/htmlrewrite'), { recursive: true });
	const original = join(dir, 'research/htmlrewrite/line\nbreak.go');
	writeFileSync(original, 'fixture');
	git('add', '.');
	git('-c', 'core.hooksPath=/dev/null', 'commit', '--no-gpg-sign', '-qm', 'base');
	const base = git('rev-parse', 'HEAD');
	renameSync(original, join(dir, 'documentation.txt'));
	git('add', '-A');
	git('-c', 'core.hooksPath=/dev/null', 'commit', '--no-gpg-sign', '-qm', 'move');
	const head = git('rev-parse', 'HEAD');
	const script = fileURLToPath(new URL('./ci-gate.mjs', import.meta.url));
	const output = join(dir, 'selection');
	const good = spawnSync(process.execPath, [script, 'select'], {
		cwd: dir,
		encoding: 'utf8',
		env: { ...process.env, BASE_SHA: base, HEAD_SHA: head, GITHUB_OUTPUT: output },
	});
	assert.equal(good.status, 0, good.stderr);
	assert.equal(readFileSync(output, 'utf8'), 'rewrite=true\nbinaryen=true\nlabels=false\n');
	const missingOutput = join(dir, 'must-not-exist');
	for (const badBase of ['', 'invalid', 'f'.repeat(40)]) {
		const bad = spawnSync(process.execPath, [script, 'select'], {
			cwd: dir,
			encoding: 'utf8',
			env: { ...process.env, BASE_SHA: badBase, HEAD_SHA: head, GITHUB_OUTPUT: missingOutput },
		});
		assert.equal(bad.error, undefined);
		assert.notEqual(bad.status, 0);
		assert.ok(!existsSync(missingOutput));
	}
});
