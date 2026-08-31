extends RefCounted
class_name DshUIComponentRegistry

## Component registry for the DSHX declarative UI runtime.
##
## The registry is the ONLY place in the engine that knows which Godot node
## classes back which document types. A document produced anywhere (JSON file,
## backend payload, hand-built dictionary) stays plain data; the mapping from
## the document type string to a live Control happens exclusively here.
##
## A factory is any Callable accepting the node's props Dictionary and
## returning a fresh Control instance:
##
##     registry.register("avatar", func(props: Dictionary) -> Control:
##         var tex := TextureRect.new()
##         tex.custom_minimum_size = Vector2(24, 24)
##         return tex)
##
## Factories receive the props so that two documents of the same type can
## materialize differently at construction time (e.g. a "panel" built flat vs
## bordered). Everything the factory returns must derive from Control; the
## reconciler enforces that at call time.
##
## Built-in coverage (registered by [method register_builtins], skipped when a
## type was already overridden):
##   column -> VBoxContainer      row      -> HBoxContainer
##   text   -> Label              button   -> Button
##   panel  -> PanelContainer     scroll   -> ScrollContainer
##   spacer -> Control
##
## The registry intentionally creates bare nodes with no theme; prop->property
## application (gap/padding/bg/…) lives in the Reconciler so that visual
## policy and node identity policy never share a code path.
##
## Interactive coverage (registered by [method register_interactive] via
## DshUIWidgets.register_all, skipped when already mapped — Hero's "icon"
## factory therefore wins if it registered first):
##   text_input  -> TextEdit         chip        -> Button (flat, clip_text)
##   icon_button -> Button (28x28)   dropdown    -> OptionButton
##   list        -> ItemList         icon        -> TextureRect
## Not registered: composer, file_picker, menu (PopupMenu is a Window).

## Types registered by [method register_builtins]. Exposed for tooling/tests.
const BUILTIN_TYPES := [
	"column", "row", "text", "button", "panel", "scroll", "spacer",
]

const DshWidgets := preload("res://engine/widgets.gd")

var _factories: Dictionary = {}


## Registers (or replaces) the factory for [param type].
## Fails loudly on empty type names, non-Callable values and factories that are
## not valid — a registry typo must fail at registration, not at mount time.
func register(type: String, factory: Callable) -> bool:
	var key := String(type).strip_edges()
	if key.is_empty():
		push_error("DshUIComponentRegistry.register: type name must not be empty")
		return false
	if factory == null or not factory.is_valid():
		push_error("DshUIComponentRegistry.register(%s): factory is not a valid Callable" % key)
		return false
	_factories[key] = factory
	return true


## Drops the mapping for [param type]. Returns false when it was not present.
func unregister(type: String) -> bool:
	return _factories.erase(String(type)) if _factories.has(type) else false


## True when [param type] resolves to a registered factory.
func has(type: String) -> bool:
	return _factories.has(String(type))


## Returns the factory for [param type], or an empty (invalid) Callable.
## Callers must check [method Callable.is_valid] — an unknown type is a data
## problem the caller owns (the Reconciler counts it as a skipped node).
func resolve(type: String) -> Callable:
	return _factories.get(String(type), Callable())


## All registered type names, unsorted (registry order is stable per session).
func types() -> Array:
	return _factories.keys()


## Instantiates the Control for [param type], passing [param props].
## Returns null for unknown types or when the factory returns a non-Control.
## Never throws: a bad type yields null and lets the Reconciler decide policy.
func create(type: String, props: Dictionary = {}) -> Control:
	var factory: Callable = resolve(type)
	if not factory.is_valid():
		return null
	var out: Variant = factory.call(props)
	if out is Control:
		return out
	push_error("DshUIComponentRegistry.create(%s): factory returned %s, expected Control"
			% [String(type), typeof(out)])
	return null


## Registers a builtin factory for every type that has no mapping yet, so the
## engine works out of the box while still allowing app-level overrides (call
## order: builtins first, then the app overrides specific types).
func register_builtins() -> void:
	if not has("column"):
		register("column", _builtin_column)
	if not has("row"):
		register("row", _builtin_row)
	if not has("text"):
		register("text", _builtin_text)
	if not has("button"):
		register("button", _builtin_button)
	if not has("panel"):
		register("panel", _builtin_panel)
	if not has("scroll"):
		register("scroll", _builtin_scroll)
	if not has("spacer"):
		register("spacer", _builtin_spacer)


## Registers interactive widget factories for types that have no mapping yet.
## Mapping-only: construction lives in DshUIWidgets.register_all (skip-if-has).
func register_interactive() -> void:
	DshWidgets.register_all(self)


# --- Builtin factories ------------------------------------------------------
# Each factory is deliberately stateless: every call yields a pristine node so
# that node identity is owned entirely by the Reconciler.


func _builtin_column(_props: Dictionary) -> Control:
	# Vertical stack. "gap" reaches it later as the "separation" theme constant.
	return VBoxContainer.new()


func _builtin_row(_props: Dictionary) -> Control:
	# Horizontal counterpart of "column".
	return HBoxContainer.new()


func _builtin_text(_props: Dictionary) -> Control:
	# Single-line label; "text" is applied later by the Reconciler like any
	# other prop so that first paint and re-paint run identical code paths.
	var label := Label.new()
	label.horizontal_alignment = HORIZONTAL_ALIGNMENT_LEFT
	label.vertical_alignment = VERTICAL_ALIGNMENT_CENTER
	return label


func _builtin_button(_props: Dictionary) -> Control:
	# "on_click" is wired by the Reconciler (signal policy lives there, not here).
	return Button.new()


func _builtin_panel(_props: Dictionary) -> Control:
	# PanelContainer owns a StyleBoxFlat from birth so bg/border/radius props
	# have something well-defined to mutate.
	var panel := PanelContainer.new()
	var sb := StyleBoxFlat.new()
	sb.draw_center = true
	sb.anti_aliasing = false
	sb.corner_detail = 8
	sb.shadow_size = 0
	panel.add_theme_stylebox_override("panel", sb)
	return panel


func _builtin_scroll(_props: Dictionary) -> Control:
	# Bare container; clipping/margins come from props when the app asks for them.
	var sc := ScrollContainer.new()
	sc.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	sc.vertical_scroll_mode = ScrollContainer.SCROLL_MODE_AUTO
	return sc


func _builtin_spacer(_props: Dictionary) -> Control:
	# Empty leaf: expands in stack layouts when given "expand"/ratio props.
	return Control.new()