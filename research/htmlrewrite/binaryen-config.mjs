export const variants = [
	['baseline', []],
	['o3', ['-O3']],
	['oz', ['-Oz']],
	['oz-twice', ['-Oz', '-Oz']],
	['rereloop-o3', ['--flatten', '--rereloop', '-O3']],
	['rereloop-oz-twice', ['--enable-bulk-memory-opt', '--flatten', '--rereloop', '-Oz', '-Oz']],
];

export function optimizerFlags(name, passes) {
	if (name === 'baseline') return [];
	// Preserve instructions already emitted by Rust in the stripped input.
	const features = ['--enable-bulk-memory-opt', '--enable-nontrapping-float-to-int'];
	return [...features, ...passes.filter(pass => !features.includes(pass))];
}
