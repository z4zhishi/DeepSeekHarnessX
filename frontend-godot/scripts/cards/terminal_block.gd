extends PanelContainer
class_name TerminalBlock

## ANSI 终端卡片：消费后端 view.terminal（字段 lines/exitCode）。
## 逐行渲染真实终端输出，SGR 颜色跨行持续（ANSI 状态机跨 append 保持），
## 末尾显示退出码。exitCode 缺省（-1）或工具未指明时不显示退出码行。

@onready var output_label: RichTextLabel = $%TerminalOutput
@onready var title_label: Label = $%TerminalHeader

var _buffer: String = ""
var _active_color: String = ""   # 跨 append 持续的当前 SGR 前景色

func begin(title: String) -> void:
	_buffer = ""
	_active_color = ""
	title_label.text = title
	output_label.text = ""
	visible = true

## 直接消费后端 view.terminal：{lines: [string], exitCode: int}。
func setup_terminal(title: String, lines: Array, exit_code: int) -> void:
	begin(title)
	for ln in lines:
		append_raw_ansi(str(ln) + "\n")
	finish(exit_code)

func append_ansi(segments: Array) -> void:
	for seg in segments:
		if typeof(seg) == TYPE_DICTIONARY:
			var text: String = seg.get("text", "")
			var color: String = ansi_color(seg.get("color", ""))
			if color != "":
				_buffer += "[color=" + color + "]" + _esc(text) + "[/color]"
			else:
				_buffer += _esc(text)
		else:
			_buffer += _esc(str(seg))
	output_label.text = _buffer

# 原生 ANSI SGR 流式追加：解析 \x1b[..m 颜色序列并转 BBCode 色块。
# 颜色在 _active_color 中跨调用持续，直到遇到 reset（\x1b[0m）。
func append_raw_ansi(raw: String) -> void:
	_parse_ansi(raw)
	output_label.text = _buffer

func finish(exit_code: int = -1) -> void:
	if exit_code >= 0:
		_buffer += "\n"
		if exit_code == 0:
			_buffer += "[color=#81c784]exit " + str(exit_code) + "[/color]"
		else:
			_buffer += "[color=#e57373]exit " + str(exit_code) + "[/color]"
	output_label.text = _buffer

## 解析 ANSI SGR 序列，把颜色状态推进到 _active_color，并累积 BBCode 文本。
## 未终结的 escape 序列（如残缺 \x1b[）直接丢弃，保持终端文本纯净。
func _parse_ansi(raw: String) -> void:
	var i := 0
	while i < raw.length():
		var ch := raw[i]
		if ch == "" and i + 1 < raw.length() and raw[i + 1] == "[":
			var j := i + 2
			var code := ""
			var term := ""
			while j < raw.length():
				var c := raw[j]
				if (c >= "0" and c <= "9") or c == ";" or c == "?":
					code += c
				else:
					term = c
					break
				j += 1
			if term == "m":
				_sgr(code)
				i = j + 1
				continue
			# 非 SGR escape（光标/C0 控制）直接跳过到字符序列结束或换行
			if j < raw.length():
				i = j + 1
			else:
				i = raw.length()
			continue
		_buffer += _esc(ch)
		i += 1

## 处理单个 SGR 参数串，更新 _active_color。
## 支持 8/16 色（30-37/90-97）与 256 色（38;5;N）前景；reset(0) 清空。
func _sgr(code: String) -> void:
	var params := code.split(";")
	if params.size() == 0:
		return
	var first := params[0]
	if first == "0":
		_active_color = ""
		return
	if first == "38" and params.size() >= 3 and params[1] == "5":
		var n := int(params[2])
		_active_color = _xterm256_fg(n)
		return
	var fg := _fg_color(first)
	if fg != "":
		_active_color = fg

func _c_color(idx: int) -> String:
	# xterm-256 16 色（0-15）映射到 6x6x6 cube 之外的标准 ANSI 调色板
	var palette := [
		"#000000", "#cd0000", "#00cd00", "#cdcd00",
		"#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
		"#7f7f7f", "#ff0000", "#00ff00", "#ffff00",
		"#5c5cff", "#ff00ff", "#00ffff", "#ffffff"
	]
	if idx >= 0 and idx < palette.size():
		return palette[idx]
	return ""

func _xterm256_fg(n: int) -> String:
	if n < 0 or n > 255:
		return ""
	if n < 16:
		return _c_color(n)
	if n < 232:
		var x := n - 16
		var r := x / 36
		var g := (x % 36) / 6
		var b := x % 6
		var comps := [0, 95, 135, 175, 215, 255]
		return "#%02x%02x%02x" % [comps[r], comps[g], comps[b]]
	var gray := 8 + (n - 232) * 10
	return "#%02x%02x%02x" % [gray, gray, gray]

func _fg_color(code: String) -> String:
	match code:
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
		_:
			return ""

func _esc(s: String) -> String:
	return s.replace("[", "[lb]").replace("]", "[/lb]")
