extends RefCounted
class_name DshUIWidgets

## Interactive widget factories for the DSHX UI runtime.
##
## The registry stays a mapping table; this file owns construction of the
## Composer-facing controls that are not among the 7 builtins. Factories
## receive props so construction-time attributes (wrap, flat, clip_text,
## min size) can be set at birth. Visual policy (tokens, i18n) stays out.
##
## Intentionally NOT registered: "composer" (composite root), "file_picker"
## (app-level DshFilePicker), "menu" (PopupMenu is a Window, not a Control
## child). "icon" is a generic TextureRect fallback — skip-if-has so Hero's
## DshIcons factory still wins when it registered first.

const INTERACTIVE_TYPES := [
	"text_input", "chip", "icon_button", "dropdown", "list", "icon",
]

const _SCRIPT := preload("res://engine/widgets.gd")

const _TEXT_INPUT_MIN_H := 36.0
const _ICON_BUTTON_SIZE := 28.0
const _ICON_DEFAULT_SIZE := 16.0
const _BRAND_PATH := "res://assets/brand/dshx_mark.svg"
const _ICON_PATH := "res://assets/icons/icon_%s.svg"


## Registers each interactive type that has no mapping yet (same skip-if-has
## contract as DshUIComponentRegistry.register_builtins).
static func register_all(registry) -> void:
	if registry == null:
		return
	if not registry.has("text_input"):
		registry.register("text_input", Callable(_SCRIPT, "_text_input"))
	if not registry.has("chip"):
		registry.register("chip", Callable(_SCRIPT, "_chip"))
	if not registry.has("icon_button"):
		registry.register("icon_button", Callable(_SCRIPT, "_icon_button"))
	if not registry.has("dropdown"):
		registry.register("dropdown", Callable(_SCRIPT, "_dropdown"))
	if not registry.has("list"):
		registry.register("list", Callable(_SCRIPT, "_list"))
	# Hero already registers a DshIcons-backed "icon"; never clobber it.
	if not registry.has("icon"):
		registry.register("icon", Callable(_SCRIPT, "_icon"))


static func _text_input(_props: Dictionary) -> Control:
	var edit := TextEdit.new()
	edit.wrap_mode = TextEdit.LINE_WRAPPING_BOUNDARY
	edit.custom_minimum_size = Vector2(0.0, _TEXT_INPUT_MIN_H)
	return edit


static func _chip(_props: Dictionary) -> Control:
	var btn := Button.new()
	btn.flat = true
	btn.clip_text = true
	return btn


static func _icon_button(_props: Dictionary) -> Control:
	var btn := Button.new()
	btn.flat = true
	btn.custom_minimum_size = Vector2(_ICON_BUTTON_SIZE, _ICON_BUTTON_SIZE)
	return btn


static func _dropdown(_props: Dictionary) -> Control:
	var opt := OptionButton.new()
	opt.clip_text = true
	return opt


static func _list(_props: Dictionary) -> Control:
	return ItemList.new()


static func _icon(props: Dictionary) -> Control:
	# Generic TextureRect fallback used when Hero has not registered first.
	# Construction-time name/glyph + size/glyph_size match the document
	# payloads already emitted by hero_doc / composer_doc.
	var rect := TextureRect.new()
	rect.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	rect.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	rect.mouse_filter = Control.MOUSE_FILTER_IGNORE
	var size_px := _ICON_DEFAULT_SIZE
	if props.has("size"):
		size_px = maxf(1.0, _to_f(props["size"]))
	elif props.has("glyph_size"):
		size_px = maxf(1.0, _to_f(props["glyph_size"]))
	rect.custom_minimum_size = Vector2(size_px, size_px)
	var icon_name := str(props.get("name", ""))
	if icon_name.is_empty():
		icon_name = str(props.get("glyph", ""))
	if icon_name == "brand":
		rect.texture = load(_BRAND_PATH) as Texture2D
	elif not icon_name.is_empty():
		rect.texture = load(_ICON_PATH % icon_name) as Texture2D
	return rect


static func _to_f(value: Variant) -> float:
	if value is float or value is int:
		return float(value)
	return 0.0
