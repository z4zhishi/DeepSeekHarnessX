extends RefCounted
class_name ConversationFold

## Folds SessionEnvelope stream into linear chat nodes.
## Node: {id, kind, payload, turn, step}

var _nodes: Array = []
var _turn: int = 0
var _step: int = 0
var _generating: bool = false
var _asst_i: Dictionary = {} ## turn -> index
var _reason_i: Dictionary = {} ## turn -> index
var _tool_i: Dictionary = {} ## callId -> index
var _cmd_i: Dictionary = {} ## commandId -> index
var _seen_seq: Dictionary = {} ## seq -> true; mux replay and fetch_history both feed this fold


func reset() -> void:
	_nodes.clear()
	_turn = 0
	_step = 0
	_generating = false
	_asst_i.clear()
	_reason_i.clear()
	_tool_i.clear()
	_cmd_i.clear()
	_seen_seq.clear()


func ingest_history(events: Array) -> void:
	reset()
	for ev in events:
		if ev is Dictionary:
			ingest(ev)


func ingest(env: Dictionary) -> void:
	var typ := str(env.get("type", ""))
	if typ == "":
		return
	var seq := int(env.get("seq", 0))
	# Idempotent ingest: mux replay and fetch_history both feed the same fold,
	# and late replay packets must never duplicate already-folded nodes.
	if seq > 0:
		if _seen_seq.has(seq):
			return
		_seen_seq[seq] = true
	var data := _as_dict(env.get("data", {}))
	if data.has("turn"):
		_turn = int(data["turn"])
	if data.has("step"):
		_step = int(data["step"])

	match typ:
		"user/message":
			_ingest_user(seq, data)
		"assistant/chunk":
			_ingest_chunk(seq, data)
		"assistant/message":
			_ingest_assistant_message(seq, data)
		"tool/call":
			_ingest_tool_call(seq, data)
		"tool/result":
			_ingest_tool_result(seq, data)
		"todo/write":
			_append("todo:%d" % seq, "todo", data)
		"plan/mode":
			_append("plan:%d" % seq, "plan", data)
		"goal/change":
			_append("goal:%d" % seq, "goal", data)
		"turn/start":
			_generating = true
			_turn = int(data.get("turn", _turn))
		"turn/end":
			_generating = false
			_turn = int(data.get("turn", _turn))
			_finalize_turn_assistant()
			_ingest_turn_end(seq, data)
		"step/start":
			_step = int(data.get("step", _step))
			_turn = int(data.get("turn", _turn))
		"step/end":
			_step = int(data.get("step", _step))
		"command/run":
			_ingest_command_run(seq, data)
		"command/done":
			_ingest_command_done(seq, data)
		"system/notice":
			_append("system:%d" % seq, "system", {
				"text": str(data.get("text", data.get("message", ""))),
				"reason": str(data.get("reason", "notice")),
			})
		"system/error":
			_append("turn-error:%d" % seq, "turn-error", {
				"text": str(data.get("text", data.get("message", ""))),
				"reason": "error",
			})
		_:
			pass


func nodes() -> Array:
	return _nodes


func is_generating() -> bool:
	return _generating


func seen_seq() -> Dictionary:
	return _seen_seq


func merge_seen_seq(seen: Dictionary) -> void:
	for k in seen.keys():
		_seen_seq[k] = seen[k]


func adopt(nodes: Array, seen: Dictionary = {}) -> void:
	reset()
	for n in nodes:
		if n is Dictionary:
			_nodes.append((n as Dictionary).duplicate(true))
	if not seen.is_empty():
		_seen_seq = seen.duplicate()
	_reindex()


func _reindex() -> void:
	_asst_i.clear()
	_reason_i.clear()
	_tool_i.clear()
	_cmd_i.clear()
	for i in _nodes.size():
		var n: Dictionary = _nodes[i]
		var kind := str(n.get("kind", ""))
		var payload: Dictionary = _as_dict(n.get("payload", {}))
		var turn := int(n.get("turn", 0))
		match kind:
			"assistant":
				_asst_i[turn] = i
				if bool(payload.get("streaming", false)):
					_generating = true
			"reasoning":
				_reason_i[turn] = i
			"tool":
				var cid := str(payload.get("callId", ""))
				if cid != "":
					_tool_i[cid] = i
			"command":
				var cmd := str(payload.get("commandId", ""))
				if cmd != "":
					_cmd_i[cmd] = i
		if turn > _turn:
			_turn = turn
		var step := int(n.get("step", 0))
		if step > _step:
			_step = step


func _ingest_user(seq: int, data: Dictionary) -> void:
	if data.get("message") is String and str(data.get("content", "")) == "":
		_append("user:%d" % seq, "user", {
			"text": str(data["message"]),
			"messageId": str(data.get("id", "")),
			"attachments": [],
		})
		return
	var msg := data
	if data.get("message") is Dictionary:
		msg = data["message"]
	var extracted := _extract_user(msg, data)
	var mid := str(extracted.get("messageId", ""))
	var id := "user:%s" % (mid if mid != "" else str(seq))
	_append(id, "user", extracted)


func _extract_user(msg: Dictionary, data: Dictionary) -> Dictionary:
	var text := ""
	var attachments: Array = []
	var content: Variant = msg.get("content", data.get("content", null))
	if content == null and data.has("text"):
		text = str(data.get("text", ""))
	elif content is String:
		text = content
	elif content is Array:
		for block in content:
			if block is String:
				text += block
			elif block is Dictionary:
				var bt := str(block.get("type", "text"))
				match bt:
					"text":
						if text != "":
							text += "\n"
						text += str(block.get("text", ""))
					"image":
						attachments.append({
							"path": str(block.get("attachmentId", block.get("path", ""))),
							"name": str(block.get("name", block.get("mimeType", "image"))),
						})
	var extra: Variant = msg.get("attachments", data.get("attachments", []))
	if extra is Array:
		for a in extra:
			if a is String:
				attachments.append({"path": a, "name": String(a).get_file()})
			elif a is Dictionary:
				attachments.append({
					"path": str(a.get("path", a.get("attachmentId", ""))),
					"name": str(a.get("name", str(a.get("path", "")).get_file())),
				})
	for m in _at_paths(text):
		var seen := false
		for a in attachments:
			if str(a.get("path", "")) == m:
				seen = true
				break
		if not seen:
			attachments.append({"path": m, "name": m.get_file() if m.get_file() != "" else m})
	return {
		"text": text,
		"messageId": str(msg.get("id", data.get("id", ""))),
		"attachments": attachments,
	}


func _at_paths(text: String) -> PackedStringArray:
	var out := PackedStringArray()
	var i := 0
	while i < text.length():
		var at := text.find("@", i)
		if at < 0:
			break
		var j := at + 1
		while j < text.length():
			var ch := text.unicode_at(j)
			if ch <= 32 or ch == 10 or ch == 13 or ch == 44:
				break
			j += 1
		var token := text.substr(at + 1, j - at - 1)
		if token.find("/") >= 0 or token.find("\\") >= 0 or token.find(".") >= 0:
			out.append(token)
		i = maxi(j, at + 1)
	return out


func _ingest_chunk(seq: int, data: Dictionary) -> void:
	var chunk: Dictionary = _as_dict(data.get("chunk", data))
	var ctype := str(chunk.get("type", ""))
	var text := str(chunk.get("text", chunk.get("delta", "")))
	if ctype == "text-delta" or (ctype == "" and text != ""):
		var node := _ensure_assistant(seq)
		var p: Dictionary = node["payload"]
		p["text"] = str(p.get("text", "")) + text
		p["streaming"] = true
		node["payload"] = p
		node["step"] = _step
	elif ctype == "reasoning-delta":
		var node := _ensure_reasoning(seq)
		var p: Dictionary = node["payload"]
		p["text"] = str(p.get("text", "")) + text
		p["streaming"] = true
		node["payload"] = p
		node["step"] = _step


func _ingest_assistant_message(seq: int, data: Dictionary) -> void:
	_turn = int(data.get("turn", _turn))
	_step = int(data.get("step", _step))
	var msg: Dictionary = _as_dict(data.get("message", {}))
	var full := _blocks_text(msg.get("content", []))
	var reason := _blocks_of(msg.get("content", []), "reasoning")
	if reason != "":
		var rnode := _ensure_reasoning(seq)
		var rp: Dictionary = rnode["payload"]
		if str(rp.get("text", "")) == "":
			rp["text"] = reason
		rp["streaming"] = false
		var usage_r: Dictionary = _as_dict(data.get("usage", {}))
		rp["tokens"] = int(usage_r.get("reasoningTokens", rp.get("tokens", 0)))
		rnode["payload"] = rp
	var node := _ensure_assistant(seq)
	var p: Dictionary = node["payload"]
	if full != "" and (str(p.get("text", "")) == "" or full.length() >= str(p.get("text", "")).length()):
		p["text"] = full
	p["streaming"] = false
	p["messageId"] = str(msg.get("id", p.get("messageId", "")))
	p["usage"] = _as_dict(data.get("usage", p.get("usage", {})))
	p["interrupted"] = bool(data.get("interrupted", false))
	node["payload"] = p
	node["step"] = _step


func _finalize_turn_assistant() -> void:
	if _asst_i.has(_turn):
		var node: Dictionary = _nodes[int(_asst_i[_turn])]
		var p: Dictionary = node["payload"]
		p["streaming"] = false
		node["payload"] = p
	if _reason_i.has(_turn):
		var node: Dictionary = _nodes[int(_reason_i[_turn])]
		var p: Dictionary = node["payload"]
		p["streaming"] = false
		node["payload"] = p


func _ingest_tool_call(seq: int, data: Dictionary) -> void:
	var call_id := str(data.get("callId", data.get("id", "")))
	if call_id == "":
		call_id = "anon-%d" % seq
	var payload := {
		"callId": call_id,
		"name": str(data.get("name", "")),
		"arguments": _cap_text(_args_string(data.get("arguments", "")), 24000),
		"view": _cap_view(_as_dict(data.get("view", {}))),
		"status": "running",
		"output": "",
	}
	if _tool_i.has(call_id):
		var node: Dictionary = _nodes[int(_tool_i[call_id])]
		var prev: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
		payload["expanded"] = bool(prev.get("expanded", false))
		node["payload"] = payload
		node["turn"] = _turn
		node["step"] = _step
	else:
		_append("tool:%s" % call_id, "tool", payload)
		_tool_i[call_id] = _nodes.size() - 1


func _ingest_tool_result(seq: int, data: Dictionary) -> void:
	var call_id := str(data.get("callId", ""))
	var output := ""
	var is_err := data.get("error") != null and str(data.get("error", "")) != ""
	var msg: Dictionary = _as_dict(data.get("message", {}))
	var content: Variant = msg.get("content", [])
	if content is Array:
		for block in content:
			if not (block is Dictionary):
				continue
			if str(block.get("type", "")) == "tool-result":
				if call_id == "":
					call_id = str(block.get("toolCallId", block.get("id", "")))
				if bool(block.get("isError", false)):
					is_err = true
				output += _blocks_text(block.get("content", []))
			elif str(block.get("type", "")) == "text":
				output += str(block.get("text", ""))
	var view: Dictionary = _as_dict(data.get("view", {}))
	if output == "":
		output = _view_text(view)
	if call_id == "":
		call_id = "anon-%d" % seq
	var status := "error" if is_err else "done"
	if _tool_i.has(call_id):
		var node: Dictionary = _nodes[int(_tool_i[call_id])]
		var p: Dictionary = node["payload"]
		p["status"] = status
		if not view.is_empty():
			p["view"] = _cap_view(view)
		p["output"] = _cap_text(output, 24000)
		if data.get("error") is Dictionary:
			p["error"] = data["error"]
		node["payload"] = p
		node["step"] = _step
	else:
		_append("tool:%s" % call_id, "tool", {
			"callId": call_id,
			"name": str(data.get("name", "")),
			"arguments": _cap_text(_args_string(data.get("arguments", "")), 24000),
			"view": _cap_view(view),
			"status": status,
			"output": _cap_text(output, 24000),
		})
		_tool_i[call_id] = _nodes.size() - 1


func _ingest_turn_end(seq: int, data: Dictionary) -> void:
	var reason: Dictionary = _as_dict(data.get("reason", {}))
	var kind := str(reason.get("kind", "completed"))
	if kind == "completed" or kind == "":
		return
	# Fallback copy is chosen at bind time by system_row via i18n; the fold only
	# carries the backend message and the reason marker.
	var text := str(reason.get("message", "")).strip_edges()
	var node_kind := "system"
	if kind == "error":
		node_kind = "turn-error"
	_append("%s:%d" % [node_kind, seq], node_kind, {
		"text": text,
		"reason": kind,
	})


func _ingest_command_run(seq: int, data: Dictionary) -> void:
	var cid := str(data.get("commandId", ""))
	if cid == "":
		cid = "cmd-%d" % seq
	var payload := {
		"commandId": cid,
		"name": str(data.get("name", "")),
		"args": str(data.get("args", "")),
		"status": "running",
		"text": "",
	}
	if _cmd_i.has(cid):
		var node: Dictionary = _nodes[int(_cmd_i[cid])]
		node["payload"] = payload
	else:
		_append("cmd:%s" % cid, "command", payload)
		_cmd_i[cid] = _nodes.size() - 1


func _ingest_command_done(seq: int, data: Dictionary) -> void:
	var cid := str(data.get("commandId", ""))
	var status := str(data.get("kind", "success"))
	var text := str(data.get("text", ""))
	if cid != "" and _cmd_i.has(cid):
		var node: Dictionary = _nodes[int(_cmd_i[cid])]
		var p: Dictionary = node["payload"]
		p["status"] = status
		p["text"] = text
		node["payload"] = p
		return
	if cid == "":
		cid = "cmd-%d" % seq
	_append("cmd:%s" % cid, "command", {
		"commandId": cid,
		"name": str(data.get("name", "")),
		"args": str(data.get("args", "")),
		"status": status,
		"text": text,
	})
	_cmd_i[cid] = _nodes.size() - 1


func _ensure_assistant(seq: int) -> Dictionary:
	if _asst_i.has(_turn):
		return _nodes[int(_asst_i[_turn])]
	_append("asst:%d:%d" % [_turn, seq], "assistant", {
		"text": "",
		"messageId": "",
		"usage": {},
		"streaming": true,
	})
	_asst_i[_turn] = _nodes.size() - 1
	return _nodes[_nodes.size() - 1]


func _ensure_reasoning(seq: int) -> Dictionary:
	if _reason_i.has(_turn):
		return _nodes[int(_reason_i[_turn])]
	_append("reason:%d:%d" % [_turn, seq], "reasoning", {
		"text": "",
		"streaming": true,
		"expanded": false,
	})
	_reason_i[_turn] = _nodes.size() - 1
	return _nodes[_nodes.size() - 1]


func _append(id: String, kind: String, payload: Dictionary) -> void:
	_nodes.append({
		"id": id,
		"kind": kind,
		"payload": payload,
		"turn": _turn,
		"step": _step,
	})


func _blocks_text(content: Variant) -> String:
	if content is String:
		return content
	if not (content is Array):
		return ""
	var parts: PackedStringArray = PackedStringArray()
	for block in content:
		if block is String:
			parts.append(block)
		elif block is Dictionary and str(block.get("type", "text")) == "text":
			parts.append(str(block.get("text", "")))
	return "\n".join(parts)


func _blocks_of(content: Variant, typ: String) -> String:
	if not (content is Array):
		return ""
	var parts: PackedStringArray = PackedStringArray()
	for block in content:
		if block is Dictionary and str(block.get("type", "")) == typ:
			parts.append(str(block.get("text", "")))
	return "\n".join(parts)


func _args_string(v: Variant) -> String:
	if v is Dictionary or v is Array:
		return JSON.stringify(v)
	return str(v)


func _view_text(view: Dictionary) -> String:
	if view.is_empty():
		return ""
	var kind := str(view.get("kind", ""))
	if kind == "terminal":
		var term: Dictionary = _as_dict(view.get("terminal", {}))
		var lines: Variant = term.get("lines", [])
		if lines is Array:
			var ps := PackedStringArray()
			for ln in lines:
				ps.append(str(ln))
			return "\n".join(ps)
		return ""
	if kind == "diff":
		return str(view.get("text", ""))
	return str(view.get("text", view.get("raw", "")))


func _as_dict(v: Variant) -> Dictionary:
	if v is Dictionary:
		return v
	if v is String:
		var parsed: Variant = JSON.parse_string(v)
		if parsed is Dictionary:
			return parsed
	return {}


func _cap_text(s: String, cap: int) -> String:
	if s.length() <= cap:
		return s
	var keep := cap / 2
	return s.substr(0, keep) + "\n…\n" + s.substr(s.length() - keep)


func _cap_view(view: Dictionary) -> Dictionary:
	if view.is_empty():
		return view
	var out := view.duplicate(true)
	if str(out.get("kind", "")) == "terminal":
		var term: Dictionary = _as_dict(out.get("terminal", {}))
		var lines: Variant = term.get("lines", [])
		if lines is Array and (lines as Array).size() > 240:
			var arr: Array = lines
			var kept: Array = []
			for i in 120:
				kept.append(arr[i])
			kept.append("… %d lines omitted …" % (arr.size() - 240))
			for i in range(arr.size() - 120, arr.size()):
				kept.append(arr[i])
			term["lines"] = kept
			out["terminal"] = term
	if str(out.get("text", "")).length() > 24000:
		out["text"] = _cap_text(str(out.get("text", "")), 24000)
	return out
