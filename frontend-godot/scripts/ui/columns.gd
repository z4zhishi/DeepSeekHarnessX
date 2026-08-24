extends RefCounted
class_name DshColumns

## Frozen CK ui-layout/columns.ts concession chain.

const CENTER_MIN := 640.0
const SIDEBAR_MIN := 264.0
const SIDEBAR_MAX := 420.0
const SIDEBAR_DEFAULT := 280.0
const SIDEBAR_COLLAPSED := 56.0
const SIDEBAR_AUTO_COLLAPSE := 1024.0
const DETAILS_MIN := 300.0
const DETAILS_MAX := 520.0
const DETAILS_DEFAULT := 360.0

static func clamp_width(px: float, mn: float, mx: float) -> float:
	return minf(mx, maxf(mn, float(roundi(px))))


static func compute_columns(viewport: float, sidebar_pref: float, details_pref: float) -> Dictionary:
	var s := SIDEBAR_COLLAPSED if sidebar_pref == 0.0 else clamp_width(sidebar_pref, SIDEBAR_MIN, SIDEBAR_MAX)
	var d0 := 0.0 if details_pref == 0.0 else clamp_width(details_pref, DETAILS_MIN, DETAILS_MAX)

	if s + d0 + CENTER_MIN <= viewport:
		return {"sidebar": s, "center": viewport - s - d0, "details": d0}

	var d1 := 0.0 if d0 == 0.0 else maxf(DETAILS_MIN, viewport - s - CENTER_MIN)
	if s + d1 + CENTER_MIN <= viewport:
		return {"sidebar": s, "center": CENTER_MIN, "details": d1}

	return {"sidebar": s, "center": maxf(0.0, viewport - s), "details": 0.0}
