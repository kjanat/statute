// @ts-check
import { spawnSync } from 'node:child_process';
import { appendFileSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const source = 'public.ecr.aws/docker/library/golang';
const destination = 'ghcr.io/kjanat/golang';

/** @param {string} mod */
export function goTags(mod) {
	const language = /^go (1\.\d+)(?:\.\d+)?\s*$/m.exec(mod)?.[1];
	const toolchain = /^toolchain go(1\.\d+\.\d+)\s*$/m.exec(mod)?.[1];
	if (!language || !toolchain || !toolchain.startsWith(`${language}.`)) {
		throw new Error('go.mod must declare matching stable Go language and toolchain versions');
	}
	return [language, toolchain].flatMap((version) => ['bookworm', 'alpine'].map((variant) => `${version}-${variant}`));
}

/**
 * @param {string[]} tags
 * @param {(args: string[]) => string} run
 * @param {(message: string) => void} report
 */
export function syncImages(tags, run, report) {
	if (!tags.length || tags.some((tag) => !/^1\.\d+(?:\.\d+)?-(?:bookworm|alpine)$/.test(tag))) {
		throw new Error('expected stable Go bookworm or alpine tags');
	}
	/** @param {string} image */
	const inspect = (image) => {
		const digest = run([
			'inspect',
			'--authfile',
			'/auth/auth.json',
			'--no-tags',
			'--retry-times',
			'3',
			'--format',
			'{{.Digest}}',
			`docker://${image}`,
		]).trim();
		if (!/^sha256:[a-f0-9]{64}$/.test(digest)) throw new Error(`invalid digest for ${image}`);
		return digest;
	};
	const images = tags.map((tag) => ({ tag, digest: inspect(`${source}:${tag}`) }));
	for (const { tag, digest } of images) {
		run([
			'copy',
			'--authfile',
			'/auth/auth.json',
			'--all',
			'--preserve-digests',
			'--retry-times',
			'3',
			`docker://${source}@${digest}`,
			`docker://${destination}:${tag}`,
		]);
		if (inspect(`${destination}:${tag}`) !== digest) throw new Error(`destination digest differs for ${tag}`);
		report(`FROM ${destination}:${tag}@${digest}`);
	}
}

export function main() {
	const tags = goTags(readFileSync(new URL('../go.mod', import.meta.url), 'utf8'));
	const token = process.env.GH_TOKEN;
	const username = process.env.GH_ACTOR;
	const image = process.env.SKOPEO_IMAGE;
	if (!token || !username) throw new Error('GH_TOKEN and GH_ACTOR are required');
	if (!image || !/^quay\.io\/skopeo\/stable@sha256:[a-f0-9]{64}$/.test(image)) {
		throw new Error('SKOPEO_IMAGE must select quay.io/skopeo/stable by digest');
	}
	const auth = mkdtempSync(join(tmpdir(), 'statute-go-image-auth-'));
	const env = { ...process.env };
	delete env.GH_TOKEN;
	/** @param {string[]} args @param {string} [input] */
	const run = (args, input) => {
		const result = spawnSync('docker', ['run', '--rm', '-i', '--volume', `${auth}:/auth`, image, ...args], {
			encoding: 'utf8',
			input,
			env,
			timeout: 20 * 60 * 1000,
			maxBuffer: 16 * 1024 * 1024,
		});
		if (result.error || result.status !== 0) {
			throw new Error(
				`skopeo ${args[0]} failed: ${String(result.error?.message ?? result.stderr).replaceAll(token, '[redacted]')}`,
			);
		}
		return result.stdout;
	};
	try {
		run(
			['login', '--authfile', '/auth/auth.json', '--username', username, '--password-stdin', 'ghcr.io'],
			`${token}\n`,
		);
		syncImages(tags, run, (line) => {
			console.log(line);
			if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `\`${line}\`\n\n`);
		});
	} finally {
		rmSync(auth, { recursive: true, force: true });
	}
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	try {
		main();
	} catch (error) {
		console.error(error instanceof Error ? error.message : String(error));
		process.exitCode = 1;
	}
}
