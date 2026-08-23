extends PanelContainer
class_name DiffBlock

## 真 Unified Diff 卡片：消费后端 view.diffs（字段 path/old/new）。
## 渲染：path 头 + ---/+++ + @@ hunk 头 + 双栏（old/new 行号对齐）+/ - 着色。
## 支持两种入口：
##   - setup_diff(path, old, new)  直接消费后端字段（推荐）
##   - setup(diff_text, title)     从已拼好的 unified diff 文本解析渲染（main.gd 现有调用）
## 两者都产出真行号双栏 diff。折叠按钮可收起正文。

@onready var text_label: RichTextLabel = $%DiffText
@onready var header_label: Label = $%DiffHeader
@onready var toggle_btn: Button = $%ToggleBtn

var _expanded: bool = true

func _ready() -> void:
	toggle_btn.pressed.connect(_toggle)

## 直接消费后端 view.diffs：{path, old, new}。
func setup_diff(path: String, old: String, new: String) -> void:
	header_label.text = path if path != "" else "Diff"
	text_label.text = _build_diff(path, old, new)
	text_label.fit_content = true

## 兼容 main.gd：从已拼好的 unified diff 文本渲染真行号双栏。
func setup(diff_text: String, title: String = "Diff") -> void:
	header_label.text = title
	text_label.text = _build_text_diff(diff_text)
	text_label.fit_content = true

func _toggle() -> void:
	_expanded = not _expanded
	text_label.visible = _expanded
	toggle_btn.text = "Collapse" if _expanded else "Expand"

## 由 path/old/new 重建 true unified diff：path 头 + @@ hunk + 双栏行号 + 着色。
func _build_diff(path: String, old: String, new: String) -> String:
	var bb := ""
	if path != "":
		bb += "[color=#b0bec5]" + _esc("--- a/" + path) + "[/color]\n"
		bb += "[color=#b0bec5]" + _esc("+++ b/" + path) + "[/color]\n"
	var old_lines := _split_nonempty(old)
	var new_lines := _split_nonempty(new)
	bb += "[color=#b0bec5]" + _esc("@@ -1,%d +1,%d @@[/color]" % [old_lines.size(), new_lines.size()]) + "\n"
	bb += _render_pairs(old_lines, new_lines, 1, 1)
	return bb

## 解析 main.gd 拼好的 unified diff 文本，渲染带行号双栏。
## 识别 "---/+++/@@" 头与 +/-/空格 行；逐 hunk 维护行号。
func _build_text_diff(text: String) -> String:
	var bb := ""
	var old_no := 0
	var new_no := 0
	var in_hunk := false
	for raw in text.split("\n"):
		var line := raw.strip_edges()
		if line.begins_with("diff --git") or line.begins_with("--- a/") or line.begins_with("+++ b/"):
			bb += "[color=#b0bec5]" + _esc(line) + "[/color]\n"
			continue
		if line.begins_with("@@"):
			bb += "[color=#b0bec5]" + _esc(line) + "[/color]\n"
			in_hunk = true
			old_no = 1
			new_no = 1
			continue
		if not in_hunk:
			bb += _esc(line) + "\n"
			continue
		if line.begins_with("+"):
			bb += "[color=#81c784]" + _esc("+%d  " % new_no) + _esc(line.substr(1)) + "[/color]\n"
			new_no += 1
		elif line.begins_with("-"):
			bb += "[color=#e57373]" + _esc("-%d  " % old_no) + _esc(line.substr(1)) + "[/color]\n"
			old_no += 1
		elif line.begins_with(" "):
			bb += _esc(" %d  %d  %s" % [old_no, new_no, line.substr(1)]) + "\n"
			old_no += 1
			new_no += 1
		else:
			bb += _esc(line) + "\n"
	return bb

## 双栏渲染：old 与 new 按行号对齐，删/增/上下文着色。
func _render_pairs(old_lines: Array, new_lines: Array, old_start: int, new_start: int) -> String:
	var bb := ""
	var i := 0
	var j := 0
	while i < old_lines.size() or j < new_lines.size():
		var old_hit := i < old_lines.size()
		var new_hit := j < new_lines.size()
		var same: bool = old_hit and new_hit and old_lines[i] == new_lines[j]
		if same:
			bb += _esc(" %d  %d  %s" % [i + old_start, j + new_start, old_lines[i]]) + "\n"
			i += 1
			j += 1
		elif old_hit and new_hit:
			bb += "[color=#e57373]" + _esc("-%d  %s" % [i + old_start, old_lines[i]]) + "[/color]\n"
			bb += "[color=#81c784]" + _esc("+%d  %s" % [j + new_start, new_lines[j]]) + "[/color]\n"
			i += 1
			j += 1
		elif old_hit:
			bb += "[color=#e57373]" + _esc("-%d  %s" % [i + old_start, old_lines[i]]) + "[/color]\n"
			i += 1
		else:
			bb += "[color=#81c784]" + _esc("+%d  %s" % [j + new_start, new_lines[j]]) + "[/color]\n"
			j += 1
	return bb

func _split_nonempty(s: String) -> Array:
	var out := PackedStringArray()
	for line in s.split("\n"):
		if line != "":
			out.append(line)
	return out

func _esc(s: String) -> String:
	return s.replace("[", "[lb]").replace("]", "[/lb]")
