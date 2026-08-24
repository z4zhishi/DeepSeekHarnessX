extends Node
class_name DshTokens

## Semantic tokens ported from CK ui-theme aliases. Feature scripts must
## read these (or the built Theme) — never Color("#…") literals.

enum Mode { DARK, LIGHT }

static var mode: Mode = Mode.DARK

static func is_dark() -> bool:
	return mode == Mode.DARK

static func bg_base() -> Color:
	return Color("151517") if is_dark() else Color("ffffff")

static func bg_sidebar() -> Color:
	return Color("1b1b1c") if is_dark() else Color("f9fafb")

static func bg_layer1() -> Color:
	return Color("232324") if is_dark() else Color("f9fafb")

static func bg_layer2() -> Color:
	return Color("2c2c2e") if is_dark() else Color("f1f3f5")

static func bg_layer3() -> Color:
	return Color("353638") if is_dark() else Color("e5e7eb")

static func bg_input() -> Color:
	return Color("1e1e20") if is_dark() else Color("ffffff")

static func bg_code() -> Color:
	return Color("18181a") if is_dark() else Color("f5f6f8")

static func bg_bubble() -> Color:
	return Color("2c2c2e") if is_dark() else Color("edf3fe")

static func text_primary() -> Color:
	return Color("f9fafb") if is_dark() else Color("0f1115")

static func text_secondary() -> Color:
	return Color("cfd3d6") if is_dark() else Color("4b5563")

static func text_tertiary() -> Color:
	return Color("81858c") if is_dark() else Color("6b7280")

static func text_muted() -> Color:
	return Color("55585e") if is_dark() else Color("9ca3af")

static func border_l1() -> Color:
	return Color(1, 1, 1, 0.06) if is_dark() else Color(0, 0, 0, 0.04)

static func border_l2() -> Color:
	return Color(1, 1, 1, 0.12) if is_dark() else Color(0, 0, 0, 0.10)

static func border_l3() -> Color:
	return Color(1, 1, 1, 0.16) if is_dark() else Color(0, 0, 0, 0.12)

static func border_l4() -> Color:
	return Color(1, 1, 1, 0.20) if is_dark() else Color(0, 0, 0, 0.16)

static func accent() -> Color:
	return Color("4176e6")

static func accent_hover() -> Color:
	return Color("679efe")

static func success() -> Color:
	return Color("22c55e")

static func danger() -> Color:
	return Color("ef4444")

static func warn() -> Color:
	return Color("f59e0b")

static func brand_button() -> Color:
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
const FONT_BODY := 16
const FONT_BODY_LH := 28
const FONT_CHROME := 13
const FONT_CHROME_LH := 20
const FONT_CAPTION := 12
const FONT_CAPTION_LH := 18
const FONT_MICRO := 11
const FONT_MICRO_LH := 14
const FONT_CODE := 13
const FONT_CODE_LH := 22

static func to_html(c: Color, with_alpha: bool = false) -> String:
	return "#" + c.to_html(with_alpha)


static func box(bg: Color, radius: int = RADIUS_MD, border: Color = Color.TRANSPARENT, bw: int = 0, pad := Vector4(8, 6, 8, 6)) -> StyleBoxFlat:
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
	# Compatibility/GLES3 StyleBoxFlat AA blends against black, not the parent fill.
	sb.anti_aliasing = false
	sb.anti_aliasing_size = 1.0
	sb.corner_detail = 8
	sb.shadow_size = 0
	sb.draw_center = true
	return sb
