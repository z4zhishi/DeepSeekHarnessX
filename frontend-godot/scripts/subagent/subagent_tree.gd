# 子代理树：宿主下行 host/subagent-started / host/subagent-finished
# 驱动 Tree 增删节点，展示 agent 谱系（父会话 -> 子会话）。
# 点击叶子节点可切换到对应子会话（面包屑导航）。

extends Tree
class_name SubagentTree

signal subagent_selected(session_id: String)

var _nodes: Dictionary = {}  # child_session_id -> TreeItem

func _ready() -> void:
	columns = 3
	set_column_title(0, "Session")
	set_column_title(1, "Status")
	set_column_title(2, "Stop Reason")
	item_selected.connect(_on_item_selected)
	hide_root = true

func add_subagent(parent_session_id: String, child_session_id: String) -> void:
	var item := create_item()
	item.set_text(0, child_session_id)
	item.set_text(1, "running")
	item.set_metadata(0, child_session_id)
	if _nodes.has(parent_session_id):
		_nodes[parent_session_id].add_child(item)
	_nodes[child_session_id] = item

func finish_subagent(child_session_id: String, status: String, stop_reason: String = "") -> void:
	if not _nodes.has(child_session_id):
		return
	var item: TreeItem = _nodes[child_session_id]
	item.set_text(1, status)
	item.set_custom_color(1, Color(0.3, 0.9, 0.3) if status == "ok" else Color(0.9, 0.3, 0.3))
	if stop_reason != "":
		item.set_text(2, stop_reason)
	_nodes.erase(child_session_id)

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