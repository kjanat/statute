function Sync-GoImage {
	[CmdletBinding()]
	param(
		[Parameter(Position = 0)]
		[ValidatePattern('\A[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\z')]
		[string[]] $Tags,

		[string] $Source = 'public.ecr.aws/docker/library/golang',
		[string] $Destination = 'ghcr.io/kjanat/golang',
		[string] $Username = 'kjanat'
	)

	$ErrorActionPreference = 'Stop'
	$token = gh auth token --hostname github.com
	if ($LASTEXITCODE -ne 0 -or !$token) {
		throw 'Could not obtain GitHub token.'
	}

	$skopeo = 'quay.io/skopeo/stable:latest'
	docker pull $skopeo
	if ($LASTEXITCODE -ne 0) {
		throw 'Could not update Skopeo.'
	}

	if (!$Tags) {
		$listing = docker run --rm $skopeo list-tags --retry-times 3 "docker://${Source}"
		if ($LASTEXITCODE -ne 0) {
			throw "Could not list tags for ${Source}."
		}
		$Tags = ($listing | ConvertFrom-Json).Tags
		if (!$Tags) {
			throw "No tags found for ${Source}."
		}
	}

	$authDir = [System.IO.Directory]::CreateTempSubdirectory('statute-go-images-')
	try {
		$token | docker run --rm -i -v "${authDir}:/auth" $skopeo login `
			--authfile /auth/auth.json --username $Username --password-stdin ghcr.io
		if ($LASTEXITCODE -ne 0) {
			throw 'Could not authenticate to GHCR.'
		}

		foreach ($tag in $Tags) {
			Write-Host "`nResolving ${Source}:${tag}..."

			$digest = docker run --rm $skopeo inspect `
				--no-tags --retry-times 3 --format '{{.Digest}}' `
				"docker://${Source}:${tag}"
			if ($LASTEXITCODE -ne 0) {
				throw "Could not resolve ${Source}:${tag}."
			}

			$digest = "$digest".Trim()
			if ($digest -notmatch '^sha256:[a-f0-9]{64}$') {
				throw "Unexpected digest: $digest"
			}

			docker run --rm -v "${authDir}:/auth:ro" $skopeo copy `
				--all --preserve-digests --retry-times 3 `
				--dest-authfile /auth/auth.json `
				"docker://${Source}@${digest}" `
				"docker://${Destination}:${tag}"
			if ($LASTEXITCODE -ne 0) {
				throw "Copy failed for ${tag}."
			}

			$reference = "FROM ${Destination}:${tag}@${digest} AS builder"
			Write-Host "`n$reference" -ForegroundColor Green
			if ($env:GITHUB_STEP_SUMMARY) {
				Add-Content -LiteralPath $env:GITHUB_STEP_SUMMARY -Value "$reference`n"
			}
		}
	}
	finally {
		Remove-Item -LiteralPath $authDir.FullName -Recurse -Force
	}
}
