extends RefCounted
class_name DshMarkdown

## Markdown → BBCode. Colors come from DshTokens.to_html(), never Color("#…") literals.


static func to_bbcode(md: String) -> String:
	if md == "":
		return ""
	var segs := segments(md)
	var out := PackedStringArray()
	for s in segs:
		if str(s.get("kind", "")) == "code":
			out.append(_format_fence(str(s.get("lang", "")), str(s.get("text", ""))))
		else:
			out.append(_md_block(str(s.get("text", ""))))
	return "\n".join(out)


static func segments(md: String) -> Array:
	var segs: Array = []
	var lines := md.split("\n")
	var i := 0
	var md_buf := PackedStringArray()
	while i < lines.size():
		var line: String = lines[i]
		if line.begins_with("```"):
			if md_buf.size() > 0:
				segs.append({"kind": "md", "text": "\n".join(md_buf), "lang": ""})
				md_buf = PackedStringArray()
			var lang := line.substr(3).strip_edges()
			i += 1
			var code := PackedStringArray()
			while i < lines.size() and not lines[i].begins_with("```"):
				code.append(lines[i])
				i += 1
			segs.append({"kind": "code", "text": "\n".join(code), "lang": lang})
			if i < lines.size() and lines[i].begins_with("```"):
				i += 1
			continue
		md_buf.append(line)
		i += 1
	if md_buf.size() > 0:
		segs.append({"kind": "md", "text": "\n".join(md_buf), "lang": ""})
	if segs.is_empty():
		segs.append({"kind": "md", "text": md, "lang": ""})
	return segs


static func hex(c: Color, with_alpha: bool = false) -> String:
	return "#" + c.to_html(with_alpha)


static func escape(s: String) -> String:
	return s.replace("[", "[lb]").replace("]", "[rb]")


static func _md_block(md: String) -> String:
	var lines := md.split("\n")
	var result := PackedStringArray()
	var table_rows: Array = []
	var i := 0
	while i < lines.size():
		var line: String = lines[i]
		if line.strip_edges().begins_with("|"):
			table_rows.append(line)
			i += 1
			continue
		if table_rows.size() > 0:
			result.append(_format_table(table_rows))
			table_rows.clear()
		if line.begins_with("### "):
			result.append("[font_size=16][b]%s[/b][/font_size]" % _inline(line.substr(4)))
		elif line.begins_with("## "):
			result.append("[font_size=18][b]%s[/b][/font_size]" % _inline(line.substr(3)))
		elif line.begins_with("# "):
			result.append("[font_size=21][b]%s[/b][/font_size]" % _inline(line.substr(2)))
		elif line.begins_with("#### "):
			result.append("[font_size=14][b]%s[/b][/font_size]" % _inline(line.substr(5)))
		elif line.begins_with("> "):
			var q := hex(DshTokens.text_tertiary())
			result.append("[i][color=%s]│ %s[/color][/i]" % [q, _inline(line.substr(2))])
		elif line.strip_edges() == ">" :
			result.append("")
		elif line.begins_with("- ") or line.begins_with("* ") or line.begins_with("+ "):
			result.append("  • %s" % _inline(line.substr(2)))
		elif _is_hr(line):
			result.append("[color=%s]────────────────────────────────[/color]" % hex(DshTokens.border_l3()))
		else:
			var ordered := _ordered(line)
			if ordered != "":
				result.append(ordered)
			else:
				result.append(_inline(line))
		i += 1
	if table_rows.size() > 0:
		result.append(_format_table(table_rows))
	return "\n".join(result)


static func _is_hr(line: String) -> bool:
	var s := line.strip_edges()
	if s.length() < 3:
		return false
	return s == "---" or s == "***" or s == "___" or s.replace("-", "") == "" and s.length() >= 3 and s.begins_with("-")


static func _ordered(line: String) -> String:
	var dot := line.find(". ")
	if dot <= 0 or dot > 4:
		return ""
	var num := line.substr(0, dot)
	if not num.is_valid_int():
		return ""
	return "  %s. %s" % [num, _inline(line.substr(dot + 2))]


static func _inline(text: String) -> String:
	var codes: PackedStringArray = PackedStringArray()
	var links: Array = []
	var s := text
	var re_code := RegEx.new()
	re_code.compile("`([^`]+)`")
	var cm := re_code.search(s)
	while cm != null:
		var token := "%%CODE%d%%" % codes.size()
		codes.append(cm.get_string(1))
		s = s.substr(0, cm.get_start()) + token + s.substr(cm.get_end())
		cm = re_code.search(s)
	var re_link := RegEx.new()
	re_link.compile("\\[([^\\]]+)\\]\\(([^)]+)\\)")
	var lm := re_link.search(s)
	while lm != null:
		var token := "%%LINK%d%%" % links.size()
		links.append({"text": lm.get_string(1), "url": lm.get_string(2)})
		s = s.substr(0, lm.get_start()) + token + s.substr(lm.get_end())
		lm = re_link.search(s)
	s = escape(s)
	var re_bold := RegEx.new()
	re_bold.compile("\\*\\*(.+?)\\*\\*")
	s = re_bold.sub(s, "[b]$1[/b]", true)
	var re_bold2 := RegEx.new()
	re_bold2.compile("__(.+?)__")
	s = re_bold2.sub(s, "[b]$1[/b]", true)
	var re_strike := RegEx.new()
	re_strike.compile("~~(.+?)~~")
	s = re_strike.sub(s, "[s]$1[/s]", true)
	var re_em := RegEx.new()
	re_em.compile("(?<!\\*)\\*(?!\\*)(.+?)(?<!\\*)\\*(?!\\*)")
	s = re_em.sub(s, "[i]$1[/i]", true)
	var re_em2 := RegEx.new()
	re_em2.compile("(?<!_)_(?!_)(.+?)(?<!_)_(?!_)")
	s = re_em2.sub(s, "[i]$1[/i]", true)
	var accent := hex(DshTokens.accent())
	var code_bg := hex(DshTokens.bg_code())
	var pri := hex(DshTokens.text_primary())
	for li in links.size():
		var L: Dictionary = links[li]
		var rendered := "[url=%s][u][color=%s]%s[/color][/u][/url]" % [
			str(L["url"]).replace("]", "%5D"),
			accent,
			escape(str(L["text"])),
		]
		s = s.replace("%%LINK%d%%" % li, rendered)
	for ci in codes.size():
		var rendered := "[bgcolor=%s][color=%s][code] %s [/code][/color][/bgcolor]" % [
			code_bg, pri, escape(codes[ci]),
		]
		s = s.replace("%%CODE%d%%" % ci, rendered)
	return s


static func _format_fence(lang: String, code: String) -> String:
	var label := lang.to_upper() if lang != "" else "CODE"
	var terc := hex(DshTokens.text_tertiary())
	var pri := hex(DshTokens.text_primary())
	var bg := hex(DshTokens.bg_code())
	var banner := "[color=%s][font_size=11]%s[/font_size][/color]" % [terc, escape(label)]
	return "%s\n[bgcolor=%s][color=%s][code]%s[/code][/color][/bgcolor]" % [
		banner, bg, pri, escape(code),
	]


static func _format_table(rows: Array) -> String:
	var cells_all: Array = []
	for r in rows:
		var s := str(r).strip_edges()
		if s.begins_with("|"):
			s = s.substr(1)
		if s.ends_with("|"):
			s = s.substr(0, s.length() - 1)
		var parts := s.split("|")
		var cells: PackedStringArray = PackedStringArray()
		var is_sep := true
		for p in parts:
			var cell := p.strip_edges()
			cells.append(cell)
			var stripped := cell.replace(":", "").replace("-", "").replace(" ", "")
			if stripped != "":
				is_sep = false
		if is_sep:
			continue
		cells_all.append(cells)
	if cells_all.is_empty():
		return ""
	var ncols := 1
	for c in cells_all:
		ncols = maxi(ncols, (c as PackedStringArray).size())
	var widths: PackedInt32Array = PackedInt32Array()
	widths.resize(ncols)
	for c in cells_all:
		var arr: PackedStringArray = c
		for i in arr.size():
			widths[i] = maxi(widths[i], arr[i].length())
	var terc := hex(DshTokens.text_tertiary())
	var pri := hex(DshTokens.text_primary())
	var out := PackedStringArray()
	for ri in cells_all.size():
		var arr: PackedStringArray = cells_all[ri]
		var row_str := ""
		for i in ncols:
			var cell := arr[i] if i < arr.size() else ""
			var pad := int(widths[i]) - cell.length()
			if pad > 0:
				cell += " ".repeat(pad)
			if i > 0:
				row_str += " │ "
			row_str += cell
		var color := pri if ri == 0 else terc
		out.append("[code][color=%s]%s[/color][/code]" % [color, escape(row_str)])
	return "\n".join(out)
