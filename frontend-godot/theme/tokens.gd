extends Node

## Semantic tokens ported from CK ui-theme aliases. Feature scripts must
## read these (or the built Theme) — never Color("#…") literals.
## Autoload identity is DshTokens (spec §7) — no class_name, it would collide
## with the autoload singleton. Access everything through the autoload.

enum Mode { DARK, LIGHT }

static var mode: Mode = Mode.DARK


func is_dark() -> bool:
	return mode == Mode.DARK


func bg_base() -> Color:
	return Color("0f0f10") if is_dark() else Color("fcfcfa")

func bg_sidebar() -> Color:
	return Color("121214") if is_dark() else Color("f6f6f3")

func bg_mesh_a() -> Color:
	return Color("1a2a6a") if is_dark() else Color("e8eefc")

func bg_mesh_b() -> Color:
	return Color("0e4a5a") if is_dark() else Color("e6f4f0")

func shadow_tinted() -> Color:
	return Color(0.06, 0.12, 0.28, 0.28) if is_dark() else Color(0.08, 0.14, 0.32, 0.12)

func bg_layer1() -> Color:
	return Color("232324") if is_dark() else Color("f9fafb")

func bg_layer2() -> Color:
	return Color("2c2c2e") if is_dark() else Color("f1f3f5")

func bg_layer3() -> Color:
	return Color("353638") if is_dark() else Color("e5e7eb")

func bg_input() -> Color:
	return Color("1e1e20") if is_dark() else Color("ffffff")

func bg_code() -> Color:
	return Color("18181a") if is_dark() else Color("f5f6f8")

func bg_bubble() -> Color:
	return Color("2c2c2e") if is_dark() else Color("edf3fe")

func text_primary() -> Color:
	return Color("f9fafb") if is_dark() else Color("0f1115")

func text_secondary() -> Color:
	return Color("cfd3d6") if is_dark() else Color("4b5563")

func text_tertiary() -> Color:
	return Color("81858c") if is_dark() else Color("6b7280")

func text_muted() -> Color:
	return Color("55585e") if is_dark() else Color("9ca3af")

func border_l1() -> Color:
	return Color(1, 1, 1, 0.08) if is_dark() else Color(0, 0, 0, 0.05)

func border_l2() -> Color:
	return Color(1, 1, 1, 0.12) if is_dark() else Color(0, 0, 0, 0.10)

func border_l3() -> Color:
	return Color(1, 1, 1, 0.16) if is_dark() else Color(0, 0, 0, 0.12)

func border_l4() -> Color:
	return Color(1, 1, 1, 0.20) if is_dark() else Color(0, 0, 0, 0.16)

func accent() -> Color:
	return Color("4176e6")

func accent_hover() -> Color:
	return Color("679efe")

func success() -> Color:
	return Color("22c55e")

func danger() -> Color:
	return Color("ef4444")

func warn() -> Color:
	return Color("f59e0b")

func brand_button() -> Color:
	return text_primary()

const RADIUS_SM := 4
const RADIUS_MD := 8
const RADIUS_LG := 12
const RADIUS_COMPOSER := 22
const RADIUS_PILL := 24

const SIDEBAR_MIN := 264.0
const SIDEBAR_DEFAULT := 280.0
const SIDEBAR_MAX := 420.0
const SIDEBAR_RAIL := 56.0
const SIDEBAR_AUTO_COLLAPSE := 1024.0
const CENTER_MIN := 640.0
const DETAILS_MIN := 300.0
const DETAILS_DEFAULT := 360.0
const DETAILS_MAX := 520.0
const CHAT_CONTENT_WIDTH := 748.0
const COMPOSER_MAX := 780.0

const FONT_UI := 14
const FONT_UI_LH := 22
const FONT_BODY := 17
const FONT_BODY_LH := 28
const FONT_CHROME_LG := 16
const FONT_CHROME_LG_LH := 24
const FONT_CHROME := 13
const FONT_CHROME_LH := 20
const FONT_CAPTION := 12
const FONT_CAPTION_LH := 18
const FONT_MICRO := 11
const FONT_MICRO_LH := 14
const FONT_CODE := 13
const FONT_CODE_LH := 22

const FONT_WEIGHT_MEDIUM := 500
const FONT_WEIGHT_SEMI := 600
const LETTER_MICRO := 0.08
const LETTER_LABEL := 0.12

## Motion windows (frontend-rebuild-spec §4). Only opacity/modulate is animated.
## Godot exposes no OS reduce-motion probe; flip motion_enabled to honor it.
const MOTION_QUICK := 0.1
const MOTION_BASE := 0.2
const MOTION_SLOW := 0.3

var motion_enabled := true


func fade_in(node: CanvasItem, dur: float = MOTION_BASE) -> void:
	if node == null or not motion_enabled:
		return
	node.modulate.a = 0.0
	var tw := node.create_tween()
	tw.tween_property(node, "modulate:a", 1.0, dur)


## Fades node out then invokes finished (deferred when motion is disabled).
func fade_out(node: CanvasItem, dur: float, finished: Callable) -> void:
	if node == null:
		return
	if not motion_enabled:
		if finished.is_valid():
			finished.call_deferred()
		return
	node.modulate.a = 1.0
	var tw := node.create_tween()
	tw.tween_property(node, "modulate:a", 0.0, dur)
	if finished.is_valid():
		tw.finished.connect(finished)
	else:
		tw.finished.connect(func() -> void: node.visible = false)

func to_html(c: Color, with_alpha: bool = false) -> String:
	return "#" + c.to_html(with_alpha)


func box(bg: Color, radius: int = RADIUS_MD, border: Color = Color.TRANSPARENT, bw: int = 0, pad := Vector4(8, 6, 8, 6)) -> StyleBoxFlat:
	var sb := StyleBoxFlat.new()
	sb.bg_color = bg
	if radius > 64:
		radius = 64
	sb.set_corner_radius_all(radius)
	sb.border_color = border
	sb.border_width_left = bw
	sb.border_width_top = bw
	sb.border_width_right = bw
	sb.border_width_bottom = bw
	sb.content_margin_left = pad.x
	sb.content_margin_top = pad.y
	sb.content_margin_right = pad.z
	sb.content_margin_bottom = pad.w
	sb.anti_aliasing = false
	sb.anti_aliasing_size = 1.0
	sb.corner_detail = 8
	sb.shadow_size = 0
	sb.draw_center = true
	return sb


func shadow_box(bg: Color, radius: int = RADIUS_MD, pad := Vector4(8, 6, 8, 6)) -> StyleBoxFlat:
	var sb := box(bg, radius, Color.TRANSPARENT, 0, pad)
	sb.shadow_color = shadow_tinted()
	sb.shadow_size = 16
	sb.shadow_offset = Vector2(0, 8)
	return sb


func double_bezel(outer_bg: Color, inner_bg: Color, outer_r: int = RADIUS_LG, pad_outer := Vector4(6, 6, 6, 6), pad_inner := Vector4(12, 10, 12, 10)) -> Array[StyleBoxFlat]:
	var outer := box(outer_bg, outer_r, border_l1(), 1, pad_outer)
	outer.shadow_color = shadow_tinted()
	outer.shadow_size = 12
	var inner_r := maxi(4, outer_r - 6)
	var inner := box(inner_bg, inner_r, border_l1(), 1, pad_inner)
	return [outer, inner]
