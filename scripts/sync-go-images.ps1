function Sync-GoImage {
	[CmdletBinding()]
	param(
		[Parameter(Position = 0)]
		[ValidatePattern('\A[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\z')]
		[string[]] $Tags,

		[string[]] $Source = @('public.ecr.aws/docker/library/golang', 'docker.io/library/golang'),
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
		foreach ($repository in $Source) {
			$listing = docker run --rm $skopeo list-tags --retry-times 3 "docker://${repository}"
			if ($LASTEXITCODE -ne 0) { continue }
			try {
				$Tags = ($listing | ConvertFrom-Json).Tags
			}
			catch {
				Write-Warning "Invalid tag listing from ${repository}."
				continue
			}
			if ($Tags) { break }
		}
		if (!$Tags) {
			throw 'Could not discover tags from any source registry.'
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
			$digest = $null
			$resolvedSource = $null
			foreach ($repository in $Source) {
				Write-Host "`nResolving ${repository}:${tag}..."
				$resolved = docker run --rm --entrypoint /bin/sh $skopeo -c `
					'skopeo inspect --raw --retry-times 3 "$1" > /tmp/manifest.json && skopeo manifest-digest /tmp/manifest.json' `
					-- "docker://${repository}:${tag}"
				if ($LASTEXITCODE -ne 0) { continue }
				$resolved = "$resolved".Trim()
				if ($resolved -notmatch '^sha256:[a-f0-9]{64}$') { continue }
				$digest = $resolved
				$resolvedSource = $repository
				break
			}
			if (!$digest) {
				throw "Could not resolve ${tag} from any source registry."
			}

			$copied = $false
			$copySources = @($resolvedSource) + @($Source | Where-Object { $_ -ne $resolvedSource })
			foreach ($repository in $copySources) {
				docker run --rm -v "${authDir}:/auth:ro" $skopeo copy `
					--all --preserve-digests --retry-times 3 `
					--dest-authfile /auth/auth.json `
					"docker://${repository}@${digest}" `
					"docker://${Destination}:${tag}"
				if ($LASTEXITCODE -eq 0) {
					$copied = $true
					break
				}
			}
			if (!$copied) {
				throw "Copy failed for ${tag} from every source registry."
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
