extends RefCounted
class_name DshIcons

const DIR := "res://assets/icons/"
const BRAND := "res://assets/brand/dshx_mark.svg"

static func texture(name: String) -> Texture2D:
	return load(DIR + "icon_" + name + ".svg") as Texture2D


static func brand() -> Texture2D:
	return load(BRAND) as Texture2D


static func paint(node: CanvasItem, primary := false) -> void:
	if node == null:
		return
	node.modulate = DshTokens.text_primary() if primary else DshTokens.text_secondary()


static func apply(rect: TextureRect, name: String, size_px := 16.0, primary := false) -> void:
	if rect == null:
		return
	rect.texture = texture(name)
	rect.custom_minimum_size = Vector2(size_px, size_px)
	rect.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	rect.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	rect.mouse_filter = Control.MOUSE_FILTER_IGNORE
	paint(rect, primary)


static func apply_brand(rect: TextureRect, size_px := 24.0) -> void:
	if rect == null:
		return
	rect.texture = brand()
	rect.custom_minimum_size = Vector2(size_px, size_px)
	rect.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	rect.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	rect.mouse_filter = Control.MOUSE_FILTER_IGNORE
	paint(rect, true)
