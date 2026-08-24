extends PanelContainer
class_name DiffBlock

@onready var text_label: RichTextLabel = %DiffText
@onready var header_label: Label = %DiffHeader
@onready var toggle_btn: Button = %ToggleBtn
@onready var copy_btn: Button = %CopyBtn
@onready var icon_rect: TextureRect = %Icon

const MAX_PAIR := 400000

var _expanded: bool = true
var _raw: String = ""
var _plain: String = ""


func _ready() -> void:
	_apply_style()
	if not toggle_btn.pressed.is_connected(_toggle):
		toggle_btn.pressed.connect(_toggle)
	if not copy_btn.pressed.is_connected(_copy):
		copy_btn.pressed.connect(_copy)
	text_label.bbcode_enabled = true
	text_label.fit_content = true
	text_label.scroll_active = false
	text_label.autowrap_mode = TextServer.AUTOWRAP_OFF
	text_label.selection_enabled = true
	text_label.add_theme_font_override("normal_font", DshThemeBuilder.code_font())
	text_label.add_theme_font_override("mono_font", DshThemeBuilder.code_font())
	text_label.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CODE)
	icon_rect.texture = load("res://assets/icons/icon_diff.svg") as Texture2D
	icon_rect.modulate = DshTokens.text_secondary()
	copy_btn.icon = load("res://assets/icons/icon_copy.svg") as Texture2D
	copy_btn.flat = true
	copy_btn.tooltip_text = _t("chat.copy", "Copy")
	toggle_btn.flat = true
	toggle_btn.text = _t("chat.collapse", "Collapse")


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		_apply_style()


func bind(node: Dictionary) -> void:
	setup_from_view(_as_dict(node.get("payload", {})).get("view", node.get("payload", {})))


func setup_from_view(view: Variant) -> void:
	var v := _as_dict(view)
	var diffs: Variant = v.get("diffs", [])
	if diffs is Array and (diffs as Array).size() > 0:
		_setup_diffs(diffs)
		return
	var text := str(v.get("text", v.get("raw", "")))
	if text != "":
		setup_text(text, str(v.get("path", v.get("title", _t("chat.diff", "Diff")))))
		return
	setup_diff(str(v.get("path", "")), str(v.get("old", "")), str(v.get("new", "")))


func setup_diff(path: String, old: String, new: String) -> void:
	header_label.text = path if path != "" else _t("chat.fileChanges", "File changes")
	_plain = _unified_plain(path, old, new)
	_raw = _render_hunks([{ "path": path, "old": old, "new": new }])
	text_label.text = _raw


func setup_text(diff_text: String, title: String = "") -> void:
	header_label.text = title if title != "" else _t("chat.diff", "Diff")
	_plain = diff_text
	if _looks_unified(diff_text):
		_raw = _render_unified(diff_text)
	else:
		_raw = _render_hunks([{ "path": title, "old": "", "new": diff_text }])
	text_label.text = _raw


func _setup_diffs(diffs: Array) -> void:
	var title := _t("chat.fileChanges", "File changes")
	if diffs.size() == 1 and diffs[0] is Dictionary:
		var p := str(diffs[0].get("path", ""))
		if p != "":
			title = p
	header_label.text = title
	var hunks: Array = []
	var plains := PackedStringArray()
	for d in diffs:
		if d is Dictionary:
			hunks.append(d)
			plains.append(_unified_plain(str(d.get("path", "")), str(d.get("old", "")), str(d.get("new", ""))))
	_plain = "\n".join(plains)
	_raw = _render_hunks(hunks)
	text_label.text = _raw


func _apply_style() -> void:
	add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_code(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(12, 8, 12, 8)
	))
	header_label.add_theme_color_override("font_color", DshTokens.text_primary())
	header_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	if icon_rect:
		icon_rect.modulate = DshTokens.text_secondary()
	if is_node_ready() and text_label and _raw != "":
		text_label.add_theme_color_override("default_color", DshTokens.text_secondary())


func _toggle() -> void:
	_expanded = not _expanded
	text_label.visible = _expanded
	toggle_btn.text = _t("chat.collapse", "Collapse") if _expanded else _t("chat.expand", "Expand")


func _copy() -> void:
	if _plain != "":
		DisplayServer.clipboard_set(_plain)
		copy_btn.tooltip_text = _t("chat.copied", "Copied")


func _render_hunks(hunks: Array) -> String:
	var bb := PackedStringArray()
	for h in hunks:
		if not (h is Dictionary):
			continue
		var path := str(h.get("path", ""))
		var old_s := str(h.get("old", ""))
		var new_s := str(h.get("new", ""))
		if path != "":
			bb.append("[color=%s]%s[/color]" % [_c(DshTokens.text_tertiary()), DshMarkdown.escape("--- a/" + path)])
			bb.append("[color=%s]%s[/color]" % [_c(DshTokens.text_tertiary()), DshMarkdown.escape("+++ b/" + path)])
		var old_lines := _split_lines(old_s)
		var new_lines := _split_lines(new_s)
		bb.append("[color=%s]%s[/color]" % [
			_c(DshTokens.accent()),
			DshMarkdown.escape("@@ -1,%d +1,%d @@" % [old_lines.size(), new_lines.size()]),
		])
		bb.append(_render_ops(_diff_ops(old_lines, new_lines)))
	return "\n".join(bb)


func _render_unified(text: String) -> String:
	var bb := PackedStringArray()
	var old_no := 0
	var new_no := 0
	var in_hunk := false
	for raw in text.split("\n"):
		var line := raw
		if line.begins_with("diff --git") or line.begins_with("index ") or line.begins_with("--- ") or line.begins_with("+++ "):
			bb.append("[color=%s]%s[/color]" % [_c(DshTokens.text_tertiary()), DshMarkdown.escape(line)])
			continue
		if line.begins_with("@@"):
			bb.append("[color=%s]%s[/color]" % [_c(DshTokens.accent()), DshMarkdown.escape(line)])
			in_hunk = true
			old_no = 1
			new_no = 1
			continue
		if not in_hunk:
			bb.append(DshMarkdown.escape(line))
			continue
		if line.begins_with("+"):
			bb.append(_line_bb("ins", 0, new_no, line.substr(1)))
			new_no += 1
		elif line.begins_with("-"):
			bb.append(_line_bb("del", old_no, 0, line.substr(1)))
			old_no += 1
		elif line.begins_with("\\"):
			bb.append("[color=%s]%s[/color]" % [_c(DshTokens.text_muted()), DshMarkdown.escape(line)])
		else:
			var body := line.substr(1) if line.begins_with(" ") else line
			bb.append(_line_bb("eq", old_no, new_no, body))
			old_no += 1
			new_no += 1
	return "\n".join(bb)


func _render_ops(ops: Array) -> String:
	var bb := PackedStringArray()
	for op in ops:
		if not (op is Dictionary):
			continue
		bb.append(_line_bb(str(op.get("tag", "eq")), int(op.get("old_no", 0)), int(op.get("new_no", 0)), str(op.get("text", ""))))
	return "\n".join(bb)


func _line_bb(tag: String, old_no: int, new_no: int, text: String) -> String:
	var del_bg := DshTokens.danger()
	del_bg.a = 0.16
	var ins_bg := DshTokens.success()
	ins_bg.a = 0.16
	var body := DshMarkdown.escape(text)
	match tag:
		"del":
			var gutter := DshMarkdown.escape("-%4d      " % old_no)
			return "[bgcolor=%s][color=%s]%s%s[/color][/bgcolor]" % [_c(del_bg, true), _c(DshTokens.danger()), gutter, body]
		"ins":
			var gutter := DshMarkdown.escape("+     %4d  " % new_no)
			return "[bgcolor=%s][color=%s]%s%s[/color][/bgcolor]" % [_c(ins_bg, true), _c(DshTokens.success()), gutter, body]
		_:
			var gutter := DshMarkdown.escape(" %4d  %4d  " % [old_no, new_no])
			return "[color=%s]%s%s[/color]" % [_c(DshTokens.text_secondary()), gutter, body]


func _diff_ops(a: PackedStringArray, b: PackedStringArray) -> Array:
	var n := a.size()
	var m := b.size()
	if n == 0 and m == 0:
		return []
	if n * m > MAX_PAIR:
		return _naive_ops(a, b)
	var matches: Array = []
	_collect_matches(a, b, 0, n, 0, m, matches)
	matches.sort_custom(func(x, y): return int(x.x) < int(y.x))
	var ops: Array = []
	var i := 0
	var j := 0
	for mb in matches:
		var ai := int(mb.x)
		var bj := int(mb.y)
		var sz := int(mb.z)
		while i < ai:
			ops.append({"tag": "del", "old_no": i + 1, "new_no": 0, "text": a[i]})
			i += 1
		while j < bj:
			ops.append({"tag": "ins", "old_no": 0, "new_no": j + 1, "text": b[j]})
			j += 1
		for k in sz:
			ops.append({"tag": "eq", "old_no": i + 1, "new_no": j + 1, "text": a[i]})
			i += 1
			j += 1
	while i < n:
		ops.append({"tag": "del", "old_no": i + 1, "new_no": 0, "text": a[i]})
		i += 1
	while j < m:
		ops.append({"tag": "ins", "old_no": 0, "new_no": j + 1, "text": b[j]})
		j += 1
	return ops


func _collect_matches(a: PackedStringArray, b: PackedStringArray, alo: int, ahi: int, blo: int, bhi: int, out: Array) -> void:
	var lm := _longest_match(a, b, alo, ahi, blo, bhi)
	if lm.z <= 0:
		return
	if lm.x > alo and lm.y > blo:
		_collect_matches(a, b, alo, lm.x, blo, lm.y, out)
	out.append(lm)
	if lm.x + lm.z < ahi and lm.y + lm.z < bhi:
		_collect_matches(a, b, lm.x + lm.z, ahi, lm.y + lm.z, bhi, out)


func _longest_match(a: PackedStringArray, b: PackedStringArray, alo: int, ahi: int, blo: int, bhi: int) -> Vector3i:
	var b2j: Dictionary = {}
	for j in range(blo, bhi):
		var line := b[j]
		if not b2j.has(line):
			b2j[line] = PackedInt32Array()
		var arr: PackedInt32Array = b2j[line]
		arr.append(j)
		b2j[line] = arr
	var best_i := alo
	var best_j := blo
	var best_size := 0
	var j2len: Dictionary = {}
	for i in range(alo, ahi):
		var new_j2len: Dictionary = {}
		var hits: PackedInt32Array = b2j.get(a[i], PackedInt32Array())
		for j in hits:
			if j < blo:
				continue
			if j >= bhi:
				break
			var k: int = int(j2len.get(j - 1, 0)) + 1
			new_j2len[j] = k
			if k > best_size:
				best_i = i - k + 1
				best_j = j - k + 1
				best_size = k
		j2len = new_j2len
	return Vector3i(best_i, best_j, best_size)


func _naive_ops(a: PackedStringArray, b: PackedStringArray) -> Array:
	var ops: Array = []
	var i := 0
	var n := mini(a.size(), b.size())
	while i < n and a[i] == b[i]:
		ops.append({"tag": "eq", "old_no": i + 1, "new_no": i + 1, "text": a[i]})
		i += 1
	var ia := a.size() - 1
	var ib := b.size() - 1
	var tail: Array = []
	while ia >= i and ib >= i and a[ia] == b[ib]:
		tail.push_front({"tag": "eq", "old_no": ia + 1, "new_no": ib + 1, "text": a[ia]})
		ia -= 1
		ib -= 1
	for k in range(i, ia + 1):
		ops.append({"tag": "del", "old_no": k + 1, "new_no": 0, "text": a[k]})
	for k in range(i, ib + 1):
		ops.append({"tag": "ins", "old_no": 0, "new_no": k + 1, "text": b[k]})
	ops.append_array(tail)
	return ops


func _split_lines(s: String) -> PackedStringArray:
	if s == "":
		return PackedStringArray()
	return s.split("\n")


func _unified_plain(path: String, old: String, new: String) -> String:
	var lines := PackedStringArray()
	if path != "":
		lines.append("--- a/" + path)
		lines.append("+++ b/" + path)
	var old_lines := _split_lines(old)
	var new_lines := _split_lines(new)
	lines.append("@@ -1,%d +1,%d @@" % [old_lines.size(), new_lines.size()])
	for op in _diff_ops(old_lines, new_lines):
		var tag := str(op.get("tag", "eq"))
		var t := str(op.get("text", ""))
		if tag == "del":
			lines.append("-" + t)
		elif tag == "ins":
			lines.append("+" + t)
		else:
			lines.append(" " + t)
	return "\n".join(lines)


func _looks_unified(text: String) -> bool:
	return text.find("\n@@") >= 0 or text.begins_with("@@") or text.begins_with("diff --git") or text.begins_with("--- ")


func _c(c: Color, with_alpha: bool = false) -> String:
	return DshMarkdown.hex(c, with_alpha)


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s


func _as_dict(v: Variant) -> Dictionary:
	if v is Dictionary:
		return v
	return {}
