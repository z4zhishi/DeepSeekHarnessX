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
	var available := maxf(0.0, viewport)
	var s := SIDEBAR_COLLAPSED if sidebar_pref == 0.0 else clamp_width(sidebar_pref, SIDEBAR_MIN, SIDEBAR_MAX)
	var d0 := 0.0 if details_pref == 0.0 else clamp_width(details_pref, DETAILS_MIN, DETAILS_MAX)

	# Preserve a usable center before honoring optional side panes. If an
	# expanded sidebar cannot coexist with the center minimum, fall back to the
	# rail; this keeps the layout fluid below the desktop three-column width.
	if s + CENTER_MIN > available:
		s = SIDEBAR_COLLAPSED

	# Details is optional for this layout pass. Hide it when it would squeeze
	# the center; the user preference remains intact for wider viewports.
	var d := d0 if s + d0 + CENTER_MIN <= available else 0.0
	return {
		"sidebar": s,
		"center": maxf(0.0, available - s - d),
		"details": d,
	}
