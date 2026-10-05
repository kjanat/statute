import assert from 'node:assert/strict';
import test from 'node:test';
import { optimizerFlags, variants } from './binaryen-config.mjs';

test('six comparisons preserve pass ordering and repeated optimization rounds', () => {
	assert.deepEqual(variants.map(([name]) => name), ['baseline', 'o3', 'oz', 'oz-twice', 'rereloop-o3', 'shuck']);
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
			name === 'oz' ? 1 : ['oz-twice', 'shuck'].includes(name) ? 2 : 0,
		);
		assert.equal(flags.filter(flag => flag === '--enable-bulk-memory-opt').length, name === 'baseline' ? 0 : 1);
		assert.equal(
			flags.filter(flag => flag === '--enable-nontrapping-float-to-int').length,
			name === 'baseline' ? 0 : 1,
		);
	}
});
