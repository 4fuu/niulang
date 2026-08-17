# Field-validation results

No multi-network candidate campaign has been completed yet. Add one redacted
record per matrix cell from `../FIELD-VALIDATION.md` and maintain the index
below. Do not convert planned or endpoint-emulated cells into field passes.

| Cell | Access class | Egress provider | Client | Port | Duration | Result | Evidence |
| --- | --- | --- | --- | ---: | ---: | --- | --- |
| Existing bounded live path | Physical long-haul uplink | Primary egress | macOS/Clash | High | 2 × 10 min | Mechanism pass; not diversity | [`20260817-primary-high-port.md`](20260817-primary-high-port.md) |

Minimum outstanding cells: two residential ISPs, two mobile carriers, one
managed/restrictive network, one additional independent path, a second egress
provider, and two 24-72-hour representative soaks.
