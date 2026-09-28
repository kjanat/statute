# Cloudflare proxy-range source snapshots

Retrieved and checked on **2026-09-28** from the exact URLs in issue #100:

| File         | Source                                | Prefixes | SHA-256 of retrieved body                                          |
| ------------ | ------------------------------------- | -------- | ------------------------------------------------------------------ |
| `ips-v4.txt` | <https://www.cloudflare.com/ips-v4/#> | 15       | `f02c6d83bc01ab0ae8577160e036d700c7455359bce054df884e5d7d9e4e9e7b` |
| `ips-v6.txt` | <https://www.cloudflare.com/ips-v6/#> | 7        | `9e9d39e3e83bad00c4decafd53c63fa62029f3d95db68de937d2be28234ca0a9` |

The HTTP response bodies had no final newline. The fixture files add that
newline, preserving all prefixes and their published order. The hashes above
describe the retrieved bodies before fixture newline normalization.

`CloudflareCIDRs()` is compared with these captured sources by offline tests.
`make check-cloudflare-cidrs` separately compares the exported bundle with the
live canonical endpoints and never writes files. The URL fragment is not sent
in the HTTP request; it is retained in source references to match the issue.

Follow [the maintenance procedure](../../docs/cloudflare.md#maintaining-the-bundled-snapshot)
to review updates, refresh the code and fixtures together, and release them.
These files are not an automatically refreshed trust source.
