extends RefCounted
class_name DshThemeBuilder

static func build(dark: bool) -> Theme:
	DshTokens.mode = DshTokens.Mode.DARK if dark else DshTokens.Mode.LIGHT
	var t := Theme.new()
	t.default_font_size = DshTokens.FONT_UI
	t.set_default_font(_ui_font())

	var bg := DshTokens.bg_base()
	var l2 := DshTokens.bg_layer2()
	var l3 := DshTokens.bg_layer3()
	var inp := DshTokens.bg_input()
	var tp := DshTokens.text_primary()
	var ts := DshTokens.text_secondary()
	var tt := DshTokens.text_tertiary()
	var b1 := DshTokens.border_l1()
	var b2 := DshTokens.border_l2()

	t.set_stylebox("panel", "PanelContainer", DshTokens.box(bg, 0, Color.TRANSPARENT, 0, Vector4.ZERO))
	t.set_stylebox("panel", "Panel", DshTokens.box(l2, DshTokens.RADIUS_MD, b1, 1, Vector4(8, 8, 8, 8)))

	t.set_color("font_color", "Label", tp)
	t.set_color("font_color", "Button", tp)
	t.set_color("font_hover_color", "Button", tp)
	t.set_color("font_pressed_color", "Button", tp)
	t.set_color("font_disabled_color", "Button", tt)
	t.set_color("font_focus_color", "Button", tp)

	var btn_n := DshTokens.box(l2, DshTokens.RADIUS_MD, b1, 1, Vector4(10, 6, 10, 6))
	var btn_h := DshTokens.box(l3, DshTokens.RADIUS_MD, b2, 1, Vector4(10, 6, 10, 6))
	t.set_stylebox("normal", "Button", btn_n)
	t.set_stylebox("hover", "Button", btn_h)
	t.set_stylebox("pressed", "Button", btn_h)
	t.set_stylebox("focus", "Button", DshTokens.box(l3, DshTokens.RADIUS_MD, DshTokens.accent(), 1, Vector4(10, 6, 10, 6)))
	t.set_stylebox("disabled", "Button", DshTokens.box(l2, DshTokens.RADIUS_MD, b1, 1, Vector4(10, 6, 10, 6)))

	t.set_stylebox("normal", "LineEdit", DshTokens.box(inp, DshTokens.RADIUS_MD, b1, 1, Vector4(8, 6, 8, 6)))
	t.set_stylebox("focus", "LineEdit", DshTokens.box(inp, DshTokens.RADIUS_MD, DshTokens.accent(), 1, Vector4(8, 6, 8, 6)))
	t.set_color("font_color", "LineEdit", tp)
	t.set_color("font_placeholder_color", "LineEdit", tt)
	t.set_color("caret_color", "LineEdit", tp)

	t.set_stylebox("normal", "TextEdit", DshTokens.box(Color(0, 0, 0, 0), 0, Color.TRANSPARENT, 0, Vector4(4, 4, 4, 4)))
	t.set_color("font_color", "TextEdit", tp)
	t.set_color("caret_color", "TextEdit", tp)

	t.set_color("default_color", "RichTextLabel", tp)
	t.set_color("font_shadow_color", "RichTextLabel", Color(0, 0, 0, 0))

	var popup_bg := DshTokens.bg_layer1()
	popup_bg.a = 0.96 if DshTokens.is_dark() else 0.98
	t.set_stylebox("panel", "PopupPanel", DshTokens.shadow_box(popup_bg, DshTokens.RADIUS_LG, Vector4(12, 12, 12, 12)))
	t.set_stylebox("panel", "PopupMenu", DshTokens.shadow_box(popup_bg, DshTokens.RADIUS_MD, Vector4(4, 4, 4, 4)))
	t.set_stylebox("hover", "PopupMenu", DshTokens.box(l3, DshTokens.RADIUS_SM, Color.TRANSPARENT, 0, Vector4(6, 4, 6, 4)))
	t.set_color("font_color", "PopupMenu", tp)
	t.set_color("font_hover_color", "PopupMenu", tp)
	t.set_color("font_accelerator_color", "PopupMenu", tt)
	t.set_color("font_disabled_color", "PopupMenu", tt)
	t.set_color("font_separator_color", "PopupMenu", ts)

	t.set_color("font_color", "ItemList", tp)
	t.set_color("font_hovered_color", "ItemList", tp)
	t.set_color("font_selected_color", "ItemList", tp)
	t.set_stylebox("panel", "ItemList", DshTokens.box(Color(0, 0, 0, 0), 0, Color.TRANSPARENT, 0, Vector4(0, 0, 0, 0)))
	t.set_stylebox("selected", "ItemList", DshTokens.box(l3, DshTokens.RADIUS_SM, Color.TRANSPARENT, 0, Vector4(6, 4, 6, 4)))
	t.set_stylebox("hovered", "ItemList", DshTokens.box(l2, DshTokens.RADIUS_SM, Color.TRANSPARENT, 0, Vector4(6, 4, 6, 4)))

	t.set_color("font_color", "Tree", tp)
	t.set_stylebox("panel", "Tree", DshTokens.box(Color(0, 0, 0, 0)))

	t.set_color("font_color", "OptionButton", tp)
	t.set_color("font_hover_color", "OptionButton", tp)
	t.set_color("font_pressed_color", "OptionButton", tp)
	t.set_color("font_disabled_color", "OptionButton", tt)
	t.set_color("font_focus_color", "OptionButton", tp)
	t.set_stylebox("normal", "OptionButton", btn_n)
	t.set_stylebox("hover", "OptionButton", btn_h)
	t.set_stylebox("pressed", "OptionButton", btn_h)
	t.set_stylebox("hover_pressed", "OptionButton", btn_h)
	t.set_stylebox("focus", "OptionButton", DshTokens.box(l3, DshTokens.RADIUS_MD, DshTokens.accent(), 1, Vector4(10, 6, 10, 6)))
	t.set_stylebox("disabled", "OptionButton", DshTokens.box(l2, DshTokens.RADIUS_MD, b1, 1, Vector4(10, 6, 10, 6)))

	t.set_color("font_color", "ProgressBar", ts)
	t.set_stylebox("background", "ProgressBar", DshTokens.box(l2, DshTokens.RADIUS_PILL, Color.TRANSPARENT, 0, Vector4(0, 0, 0, 0)))
	t.set_stylebox("fill", "ProgressBar", DshTokens.box(DshTokens.accent(), DshTokens.RADIUS_PILL, Color.TRANSPARENT, 0, Vector4(0, 0, 0, 0)))

	t.set_color("font_color", "TooltipLabel", tp)
	var tip_bg := DshTokens.bg_layer3()
	tip_bg.a = 0.98
	var tip_box := DshTokens.box(tip_bg, DshTokens.RADIUS_SM, b2, 1, Vector4(8, 4, 8, 4))
	tip_box.shadow_color = DshTokens.shadow_tinted()
	tip_box.shadow_size = 8
	t.set_stylebox("panel", "TooltipPanel", tip_box)
	return t

static func _ui_font() -> Font:
	var f := SystemFont.new()
	f.font_names = PackedStringArray(["Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC", "sans-serif"])
	return f

static func code_font() -> Font:
	var f := SystemFont.new()
	f.font_names = PackedStringArray(["Cascadia Mono", "Consolas", "JetBrains Mono", "Microsoft YaHei", "Courier New"])
	return f
