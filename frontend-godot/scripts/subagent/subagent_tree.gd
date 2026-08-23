# 子代理树：宿主下行 host/subagent-started / host/subagent-finished
# 驱动 Tree 增删节点，展示 agent 谱系（根会话 -> 子会话 -> 孙会话）。
# 修复根节点断裂：根会话也入 _nodes（add_root_session / 父缺失时自愈建档），
# 子节点挂到真父节点下形成真实父子谱系；finish 不清空 _nodes，
# 谱系保持（仅改状态色），并记录 token / 时长 两列。
# 点击叶子节点可切换到对应子会话（面包屑导航）。

extends Tree
class_name SubagentTree

signal subagent_selected(session_id: String)

var _nodes: Dictionary = {}  # session_id -> TreeItem

func _ready() -> void:
	columns = 4
	set_column_title(0, "Session")
	set_column_title(1, "Status")
	set_column_title(2, "Tokens")
	set_column_title(3, "Duration")
	set_column_expand(0, true)
	set_column_expand(1, false)
	set_column_expand(2, false)
	set_column_expand(3, false)
	set_column_clip_content(0, true)
	set_column_clip_content(1, true)
	set_column_clip_content(2, true)
	set_column_clip_content(3, true)
	item_selected.connect(_on_item_selected)
	hide_root = true

## 注册根会话（活动会话），使其进入谱系树而非悬空。
func add_root_session(session_id: String) -> void:
	if session_id == "" or _nodes.has(session_id):
		return
	var item := create_item()
	item.set_text(0, session_id)
	item.set_text(1, "active")
	item.set_metadata(0, session_id)
	_nodes[session_id] = item

## 父会话缺失时自动补一个"root"建档，保证真父子谱系不悬空。
func add_subagent(parent_session_id: String, child_session_id: String) -> void:
	if child_session_id == "":
		return
	if not _nodes.has(parent_session_id):
		# 自愈：父会话从未登记过（如程序启动前已在跑）——先建根。
		if parent_session_id != "":
			add_root_session(parent_session_id)
	var item := create_item()
	item.set_text(0, child_session_id)
	item.set_text(1, "running")
	item.set_metadata(0, child_session_id)
	if _nodes.has(parent_session_id):
		_nodes[parent_session_id].add_child(item)
	else:
		# 连父 ID 都没有：挂根下，仍保留节点（不丢弃）。
		var root := get_root()
		if root != null:
			root.add_child(item)
	_nodes[child_session_id] = item

func finish_subagent(child_session_id: String, status: String, stop_reason: String = "", tokens: int = 0, duration_ms: int = 0) -> void:
	if not _nodes.has(child_session_id):
		return
	var item: TreeItem = _nodes[child_session_id]
	item.set_text(1, status)
	item.set_custom_color(1, Color(0.3, 0.9, 0.3) if status == "ok" else Color(0.9, 0.3, 0.3))
	if stop_reason != "":
		item.set_tooltip_text(0, "Stop reason: " + stop_reason)
	if tokens > 0:
		item.set_text(2, str(tokens))
	if duration_ms > 0:
		item.set_text(3, _fmt_duration(duration_ms))
	# 不清 _nodes：谱系保持，仅状态改变。父节点移出子节点会断树，故不移除。

func _fmt_duration(ms: int) -> String:
	if ms < 1000:
		return str(ms) + "ms"
	var sec := ms / 1000
	if sec < 60:
		return str(sec) + "s"
	return "%dm%ds" % [sec / 60, sec % 60]

func _on_item_selected() -> void:
	var item := get_selected()
	if item == null:
		return
	var sid: String = item.get_metadata(0)
	if sid != "":
		subagent_selected.emit(sid)

func clear_all() -> void:
	clear()
	_nodes.clear()
