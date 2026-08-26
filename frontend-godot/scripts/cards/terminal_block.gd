extends PanelContainer
class_name TerminalBlock

@onready var output_label: RichTextLabel = %TerminalOutput
@onready var title_label: Label = %TerminalHeader
@onready var expand_btn: Button = %ExpandBtn
@onready var icon_rect: TextureRect = %Icon

const HEAD := 80
const TAIL := 80
const ESC := 0x1B

var _lines: PackedStringArray = PackedStringArray()
var _plain_lines: PackedStringArray = PackedStringArray()
var _expanded_all: bool = false
var _exit_code: int = -1
var _color: String = ""
var _bg: String = ""
var _bold: bool = false
var _italic: bool = false
var _underline: bool = false
var _defer_paint: bool = false
var _applying_style: bool = false


func _ready() -> void:
	_apply_style()
	output_label.bbcode_enabled = true
	output_label.fit_content = true
	output_label.scroll_active = false
	output_label.autowrap_mode = TextServer.AUTOWRAP_OFF
	output_label.selection_enabled = true
	output_label.add_theme_font_override("normal_font", DshThemeBuilder.code_font())
	output_label.add_theme_font_override("mono_font", DshThemeBuilder.code_font())
	output_label.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CODE)
	icon_rect.texture = load("res://assets/icons/icon_terminal.svg") as Texture2D
	icon_rect.modulate = DshTokens.success()
	expand_btn.flat = true
	expand_btn.visible = false
	if not expand_btn.pressed.is_connected(_toggle_cap):
		expand_btn.pressed.connect(_toggle_cap)


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED and not _applying_style:
		_apply_style()


func bind(node: Dictionary) -> void:
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else node
	setup_from_view(p.get("view", p))


func setup_from_view(view: Variant) -> void:
	var v := view as Dictionary if view is Dictionary else {}
	var term: Dictionary = v.get("terminal", {}) if v.get("terminal") is Dictionary else v
	var title := str(v.get("title", _t("chat.terminal", "Terminal")))
	var lines: Array = []
	var src: Variant = term.get("lines", v.get("lines", []))
	if src is Array:
		lines = src
	elif src is String:
		lines = (src as String).split("\n")
	var code := int(term.get("exitCode", v.get("exitCode", -1)))
	if lines.is_empty():
		var text := str(v.get("text", ""))
		if text != "":
			lines = text.split("\n")
	setup(title, lines, code)


func setup(title: String, lines: Array, exit_code: int = -1) -> void:
	begin(title)
	var use: Array = lines
	if lines.size() > 400:
		use = []
		for i in 200:
			use.append(lines[i])
		use.append("… %d lines omitted …" % (lines.size() - 400))
		for i in range(lines.size() - 200, lines.size()):
			use.append(lines[i])
	_defer_paint = true
	for ln in use:
		append_raw_ansi(str(ln))
		if not str(ln).ends_with("\n"):
			_finish_plain_line()
	_defer_paint = false
	finish(exit_code)


func begin(title: String) -> void:
	_lines = PackedStringArray()
	_plain_lines = PackedStringArray()
	_expanded_all = false
	_exit_code = -1
	_reset_sgr()
	title_label.text = title if title != "" else _t("chat.terminal", "Terminal")
	output_label.text = ""
	visible = true


func append_raw_ansi(raw: String) -> void:
	_parse_ansi(raw)
	if not _defer_paint:
		_paint()


func finish(exit_code: int = -1) -> void:
	_flush_partial()
	_exit_code = exit_code
	_paint()


func _toggle_cap() -> void:
	_expanded_all = not _expanded_all
	_paint()


func _apply_style() -> void:
	if _applying_style:
		return
	_applying_style = true
	add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_code(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(12, 8, 12, 8)
	))
	if title_label != null:
		title_label.add_theme_color_override("font_color", DshTokens.success())
		title_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	if icon_rect != null:
		icon_rect.modulate = DshTokens.success()
	_applying_style = false


func _parse_ansi(raw: String) -> void:
	var i := 0
	var n := raw.length()
	while i < n:
		var cp := raw.unicode_at(i)
		if cp == ESC and i + 1 < n and raw[i + 1] == "[":
			var j := i + 2
			var params := ""
			var term := ""
			while j < n:
				var c := raw[j]
				var uc := raw.unicode_at(j)
				if (c >= "0" and c <= "9") or c == ";" or c == "?":
					params += c
					j += 1
					continue
				if uc >= 64 and uc <= 126:
					term = c
					j += 1
					break
				j += 1
				break
			if term == "m":
				_sgr(params)
			i = j
			continue
		if cp == ESC:
			# Drop other ESC sequences (OSC/cursor) until ST or a terminator letter.
			var j := i + 1
			if j < n and raw[j] == "]":
				j += 1
				while j < n:
					var u := raw.unicode_at(j)
					if u == 7 or u == ESC:
						if u == ESC:
							j += 1
						j += 1
						break
					j += 1
				i = j
				continue
			i += 1
			continue
		if cp == 10:
			_finish_plain_line()
			i += 1
			continue
		if cp == 13:
			i += 1
			continue
		_push_char(raw[i])
		i += 1


func _push_char(ch: String) -> void:
	if _plain_lines.is_empty():
		_plain_lines.append("")
		_lines.append("")
	_plain_lines[_plain_lines.size() - 1] += ch
	_lines[_lines.size() - 1] += _styled(DshMarkdown.escape(ch))


func _finish_plain_line() -> void:
	if _plain_lines.is_empty():
		_plain_lines.append("")
		_lines.append("")
	_plain_lines.append("")
	_lines.append("")


func _flush_partial() -> void:
	if _plain_lines.size() == 0:
		return
	if _plain_lines[_plain_lines.size() - 1] == "" and _lines.size() > 0:
		_plain_lines.resize(_plain_lines.size() - 1)
		_lines.resize(_lines.size() - 1)


func _styled(escaped: String) -> String:
	var open := ""
	var close := ""
	if _bold:
		open += "[b]"
		close = "[/b]" + close
	if _italic:
		open += "[i]"
		close = "[/i]" + close
	if _underline:
		open += "[u]"
		close = "[/u]" + close
	if _color != "":
		open += "[color=%s]" % _color
		close = "[/color]" + close
	if _bg != "":
		open += "[bgcolor=%s]" % _bg
		close = "[/bgcolor]" + close
	return open + escaped + close


func _sgr(code: String) -> void:
	var params := code.split(";", false)
	if params.is_empty():
		_reset_sgr()
		return
	var i := 0
	while i < params.size():
		var p := params[i]
		if p == "" or p == "0":
			_reset_sgr()
			i += 1
			continue
		if p == "1":
			_bold = true
		elif p == "3":
			_italic = true
		elif p == "4":
			_underline = true
		elif p == "22":
			_bold = false
		elif p == "23":
			_italic = false
		elif p == "24":
			_underline = false
		elif p == "39":
			_color = ""
		elif p == "49":
			_bg = ""
		elif p == "38" or p == "48":
			var is_fg := p == "38"
			if i + 1 < params.size() and params[i + 1] == "5" and i + 2 < params.size():
				var hx := _color256(int(params[i + 2]))
				if is_fg:
					_color = hx
				else:
					_bg = hx
				i += 3
				continue
			if i + 1 < params.size() and params[i + 1] == "2" and i + 4 < params.size():
				var hx := _rgb(int(params[i + 2]), int(params[i + 3]), int(params[i + 4]))
				if is_fg:
					_color = hx
				else:
					_bg = hx
				i += 5
				continue
		elif p.is_valid_int():
			var iv := int(p)
			if (iv >= 30 and iv <= 37) or (iv >= 90 and iv <= 97):
				_color = _ansi_fg(iv)
			elif (iv >= 40 and iv <= 47) or (iv >= 100 and iv <= 107):
				_bg = _ansi_bg(iv)
		i += 1


func _reset_sgr() -> void:
	_color = ""
	_bg = ""
	_bold = false
	_italic = false
	_underline = false


func _paint() -> void:
	var n := _lines.size()
	var hidden := 0
	var body := PackedStringArray()
	if not _expanded_all and n > HEAD + TAIL + 8:
		hidden = n - HEAD - TAIL
		for i in HEAD:
			body.append(_lines[i])
		var terc := DshMarkdown.hex(DshTokens.text_tertiary())
		body.append("[color=%s]… %d lines hidden …[/color]" % [terc, hidden])
		for i in range(n - TAIL, n):
			body.append(_lines[i])
		expand_btn.visible = true
		expand_btn.text = _t("chat.expand", "Expand")
	else:
		body = _lines.duplicate()
		expand_btn.visible = hidden > 0 or (n > HEAD + TAIL + 8)
		if _expanded_all and n > HEAD + TAIL + 8:
			expand_btn.text = _t("chat.collapse", "Collapse")
			expand_btn.visible = true
		elif n <= HEAD + TAIL + 8:
			expand_btn.visible = false
	if _exit_code >= 0:
		if _exit_code == 0:
			body.append("[color=%s]● exit 0[/color]" % DshMarkdown.hex(DshTokens.success()))
		else:
			body.append("[color=%s]● exit %d[/color]" % [DshMarkdown.hex(DshTokens.danger()), _exit_code])
	output_label.text = "\n".join(body)


func _ansi_fg(code: int) -> String:
	match code:
		30, 90: return _rgb(158, 158, 158)
		31, 91: return _rgb(239, 68, 68)
		32, 92: return _rgb(34, 197, 94)
		33, 93: return _rgb(245, 158, 11)
		34, 94: return _rgb(65, 118, 230)
		35, 95: return _rgb(168, 85, 247)
		36, 96: return _rgb(6, 182, 212)
		37, 97: return _rgb(243, 244, 246)
		_: return ""


func _ansi_bg(code: int) -> String:
	var fg := code - 10
	if code >= 100:
		fg = code - 10
	return _ansi_fg(fg)


func _color256(n: int) -> String:
	if n < 16:
		var palette := [
			[0, 0, 0], [128, 0, 0], [0, 128, 0], [128, 128, 0],
			[0, 0, 128], [128, 0, 128], [0, 128, 128], [192, 192, 192],
			[128, 128, 128], [255, 0, 0], [0, 255, 0], [255, 255, 0],
			[0, 0, 255], [255, 0, 255], [0, 255, 255], [255, 255, 255],
		]
		var rgb: Array = palette[n]
		return _rgb(int(rgb[0]), int(rgb[1]), int(rgb[2]))
	if n < 232:
		var i := n - 16
		var r := (i / 36) % 6
		var g := (i / 6) % 6
		var b := i % 6
		return _rgb(_cube(r), _cube(g), _cube(b))
	var shade := 8 + (n - 232) * 10
	return _rgb(shade, shade, shade)


func _cube(v: int) -> int:
	var vals := [0, 95, 135, 175, 215, 255]
	return vals[v] if v < vals.size() else 255


func _rgb(r: int, g: int, b: int) -> String:
	return "#" + Color8(clampi(r, 0, 255), clampi(g, 0, 255), clampi(b, 0, 255)).to_html(false)


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
