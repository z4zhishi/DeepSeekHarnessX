extends RefCounted
class_name DshUILayoutEngine

## Responsive layout rule engine for the DSHX declarative UI runtime.
##
## Pure data pipeline: viewport width -> LayoutMode -> layout rules. It never
## mutates nodes and never reads nodes; the Reconciler consumes the returned
## rules as document props, which keeps the responsive pipeline one-directional
## and fully unit-testable:
##
##     viewport width ──> DshUILayoutEngine.rules_for_width()
##                            │ (gap / padding / columns / sidebar_visible …)
##                            ▼
##                    document props ──> DshUIReconciler.update() ──> Controls
##
## Mode semantics (each breakpoint is the INCLUSIVE lower bound — a width of
## exactly bp_compact is COMPACT, not NARROW):
##   WIDE    ≥ wide_breakpoint     sidebar + center + details (3 columns)
##   COMPACT ≥ compact_breakpoint  sidebar + center (details folds away)
##   NARROW  ≥ narrow_breakpoint   center only (sidebar collapsed away)
##   TINY    < narrow_breakpoint   center only, tighter spacing, scaled fonts
##
## Default breakpoints line up with theme/tokens.gd spacing constants
## (SIDEBAR_AUTO_COLLAPSE = 1024 px marks the COMPACT boundary) and the
## project's minimum window width; every breakpoint is configurable per
## instance through configure(), so tests can shrink the whole scale.

enum LayoutMode { WIDE, COMPACT, NARROW, TINY }

## Mode display names, indexed by LayoutMode. Kept as a const Array so
## mode_name() can map enum -> string without a match ladder.
const MODE_NAMES := ["WIDE", "COMPACT", "NARROW", "TINY"]

## Default breakpoints in viewport pixels (each is an inclusive lower bound).
const DEFAULT_BP_WIDE := 1280.0
const DEFAULT_BP_COMPACT := 1024.0
const DEFAULT_BP_NARROW := 640.0

## Rule templates per mode (static so tooling can read them without an
## instance). `content_width` 0 means "fill the viewport"; font_scale is a
## multiplier over the theme's base font size, only TINY shrinks it.
static var RULES := {
	LayoutMode.WIDE: {
		"mode": LayoutMode.WIDE, "mode_name": "WIDE",
		"columns": 3, "gap": 16, "padding": 24,
		"sidebar_visible": true, "details_visible": true,
		"font_scale": 1.0, "content_width": 748.0,
	},
	LayoutMode.COMPACT: {
		"mode": LayoutMode.COMPACT, "mode_name": "COMPACT",
		"columns": 2, "gap": 12, "padding": 16,
		"sidebar_visible": true, "details_visible": false,
		"font_scale": 1.0, "content_width": 640.0,
	},
	LayoutMode.NARROW: {
		"mode": LayoutMode.NARROW, "mode_name": "NARROW",
		"columns": 1, "gap": 10, "padding": 12,
		"sidebar_visible": false, "details_visible": false,
		"font_scale": 1.0, "content_width": 0.0,
	},
	LayoutMode.TINY: {
		"mode": LayoutMode.TINY, "mode_name": "TINY",
		"columns": 1, "gap": 6, "padding": 8,
		"sidebar_visible": false, "details_visible": false,
		"font_scale": 0.92, "content_width": 0.0,
	},
}

# Instance breakpoints (mutate via configure(); the defaults above hold for a
# fresh instance).
var bp_wide: float = DEFAULT_BP_WIDE
var bp_compact: float = DEFAULT_BP_COMPACT
var bp_narrow: float = DEFAULT_BP_NARROW


## Overrides the breakpoints. Values are corrected to keep the ordering
## wide > compact > narrow > 0 — a misconfigured engine must fail safe with a
## monotone scale instead of overlapping ranges.
func configure(p_wide: float, p_compact: float, p_narrow: float) -> void:
	bp_narrow = maxf(1.0, p_narrow)
	bp_compact = maxf(bp_narrow + 1.0, p_compact)
	bp_wide = maxf(bp_compact + 1.0, p_wide)


## Mode for [param width] using this engine's breakpoints (inclusive bounds).
func mode_for_width(width: float) -> LayoutMode:
	if width >= bp_wide:
		return LayoutMode.WIDE
	if width >= bp_compact:
		return LayoutMode.COMPACT
	if width >= bp_narrow:
		return LayoutMode.NARROW
	return LayoutMode.TINY


## Layout rule set for [param width]. Returns a duplicate of the template so
## callers can mutate their copy freely without corrupting the shared table.
func rules_for_width(width: float) -> Dictionary:
	return rules_for_mode(mode_for_width(width))


## Layout rules straight from the current viewport (headless-safe: reads the
## visible rect, which is writable even when no real window exists).
func rules_for_viewport(viewport: Viewport) -> Dictionary:
	if viewport == null:
		return rules_for_mode(LayoutMode.TINY)
	return rules_for_width(viewport.get_visible_rect().size.x)


## Template lookup by mode; duplicated on return so callers can mutate freely.
func rules_for_mode(mode: LayoutMode) -> Dictionary:
	var rule: Dictionary = RULES[mode]
	return rule.duplicate()


## Enum -> display name ("" for out-of-range values, never an error).
static func mode_name(mode: LayoutMode) -> String:
	if mode < 0 or mode >= MODE_NAMES.size():
		return ""
	return MODE_NAMES[mode]


## Breakpoint-free variant used by tools/probes that want the default contract
## without constructing an engine.
static func default_mode_for_width(width: float) -> LayoutMode:
	if width >= DEFAULT_BP_WIDE:
		return LayoutMode.WIDE
	if width >= DEFAULT_BP_COMPACT:
		return LayoutMode.COMPACT
	if width >= DEFAULT_BP_NARROW:
		return LayoutMode.NARROW
	return LayoutMode.TINY