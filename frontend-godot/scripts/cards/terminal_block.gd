extends PanelContainer
class_name TerminalBlock

@onready var output_label: RichTextLabel = $%TerminalOutput
@onready var title_label: Label = $%TerminalHeader

var _buffer: String = ""

func begin(title: String) -> void:
	_buffer = ""
	title_label.text = title
	output_label.text = ""
	visible = true

func append_ansi(segments: Array) -> void:
	for seg in segments:
		if typeof(seg) == TYPE_DICTIONARY:
			var text: String = seg.get("text", "")
			var color: String = ansi_color(seg.get("color", ""))
			if color != "":
				_buffer += "[color=" + color + "]" + text.replace("[", "[lb]").replace("]", "[/lb]") + "[/color]"
			else:
				_buffer += text.replace("[", "[lb]").replace("]", "[/lb]")
		else:
			_buffer += str(seg).replace("[", "[lb]").replace("]", "[/lb]")
	output_label.text = _buffer
	output_label.fit_content = true

# 原生 ANSI SGR 流式追加：解析 \x1b[..m 颜色序列并转 BBCode 色块，
# 未知/未支持序列直接丢弃（保持终端文本纯净）。
func append_raw_ansi(raw: String) -> void:
	_buffer += _parse_ansi(raw)
	output_label.text = _buffer
	output_label.fit_content = true

func finish() -> void:
	output_label.text = _buffer
	output_label.fit_content = true

func _parse_ansi(raw: String) -> String:
	var sb := ""
	var i := 0
	var active_color := ""
	while i < raw.length():
		var ch := raw[i]
		if ch == "\u001b" and i + 1 < raw.length() and raw[i + 1] == "[":
			var j := i + 2
			var code := ""
			while j < raw.length():
				var c := raw[j]
				if c == "m":
					break
				code += c
				j += 1
			if j < raw.length() and raw[j] == "m":
				active_color = _sgr_color(code)
				i = j + 1
				continue
		var esc := raw[i].replace("[", "[lb]").replace("]", "[/lb]")
		if active_color != "":
			sb += "[color=" + active_color + "]" + esc + "[/color]"
		else:
			sb += esc
		i += 1
	return sb

func _sgr_color(code: String) -> String:
	var params := code.split(";")
	for p in params:
		if p == "0" or p == "1" or p == "2":
			continue  # reset / bold / dim: 颜色继承现有（不重置）
	match code:
		"0", "00": return ""
		"30", "90": return "#9e9e9e"
		"31", "91": return "#e57373"
		"32", "92": return "#81c784"
		"33", "93": return "#ffd54f"
		"34", "94": return "#64b5f6"
		"35", "95": return "#ba68c8"
		"36", "96": return "#4dd0e1"
		"37", "97": return "#e0e0e0"
		_:
			return ""

func ansi_color(name: String) -> String:
	match name:
		"red": return "#e57373"
		"green": return "#81c784"
		"yellow": return "#ffd54f"
		"blue": return "#64b5f6"
		"magenta": return "#ba68c8"
		"cyan": return "#4dd0e1"
		_ : return ""