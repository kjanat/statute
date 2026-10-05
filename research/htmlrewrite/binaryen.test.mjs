import assert from 'node:assert/strict';
import test from 'node:test';
import { optimizerFlags, variants } from './binaryen-config.mjs';

test('six comparisons preserve pass ordering and repeated optimization rounds', () => {
	assert.deepEqual(variants.map(([name]) => name), [
		'baseline',
		'o3',
		'oz',
		'oz-twice',
		'rereloop-o3',
		'rereloop-oz-twice',
	]);
	for (const [name, passes] of variants) {
		const original = [...passes];
		const flags = optimizerFlags(name, passes);
		assert.deepEqual(passes, original);
		assert.deepEqual(
			flags.filter(flag => !flag.startsWith('--enable-')),
			passes.filter(flag => !flag.startsWith('--enable-')),
		);
		assert.equal(
			flags.filter(flag => flag === '-Oz').length,
			name === 'oz' ? 1 : ['oz-twice', 'rereloop-oz-twice'].includes(name) ? 2 : 0,
		);
		assert.equal(flags.filter(flag => flag === '--enable-bulk-memory-opt').length, name === 'baseline' ? 0 : 1);
		assert.equal(
			flags.filter(flag => flag === '--enable-nontrapping-float-to-int').length,
			name === 'baseline' ? 0 : 1,
		);
	}
});

test('rereloop-oz-twice retains its exact effective flags', () => {
	const [name, passes] = variants.find(([name]) => name === 'rereloop-oz-twice');
	assert.deepEqual(optimizerFlags(name, passes), [
		'--enable-bulk-memory-opt',
		'--enable-nontrapping-float-to-int',
		'--flatten',
		'--rereloop',
		'-Oz',
		'-Oz',
	]);
});
