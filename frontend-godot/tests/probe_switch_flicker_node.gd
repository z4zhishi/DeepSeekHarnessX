extends Node

## 探针：会话切换防抖与历史去重测试
## 验证点：
## 1. ChatList.set_nodes 幂等去重与 _is_same_nodes 早退
## 2. ConversationFold.adopt 保持 _seen_seq 幂等性
## 3. 重复历史事件不会造成节点翻倍或乱序
## 4. 模拟快速切换会话时不出现死锁或节点泄露

var _passed := 0
var _failed := 0

func _assert(cond: bool, msg: String) -> void:
	var line := ""
	if cond:
		_passed += 1
		line = "  [PASS] %s" % msg
		print(line)
	else:
		_failed += 1
		line = "  [FAIL] %s" % msg
		printerr(line)
	var f := FileAccess.open("user://flicker_probe_log.txt", FileAccess.READ_WRITE)
	if f == null:
		f = FileAccess.open("user://flicker_probe_log.txt", FileAccess.WRITE)
	if f:
		f.seek_end()
		f.store_line(line)
		f.close()

func _ready() -> void:
	var f := FileAccess.open("user://flicker_probe_log.txt", FileAccess.WRITE)
	if f:
		f.store_line("=== PROBE SWITCH FLICKER START ===")
		f.close()
	print("=== PROBE SWITCH FLICKER START ===")
	_run_tests()

func _run_tests() -> void:
	await get_tree().process_frame

	# 1. 测试 ConversationFold seen_seq 保持
	var fold1 := ConversationFold.new()
	var ev1 := {"type": "user/message", "seq": 1, "data": {"text": "hello", "turn": 1, "step": 1}}
	var ev2 := {"type": "assistant/chunk", "seq": 2, "data": {"chunk": {"type": "text-delta", "text": "world"}, "turn": 1, "step": 1}}
	var ev3 := {"type": "assistant/message", "seq": 3, "data": {"turn": 1, "step": 1, "message": {"id": "m1", "role": "assistant", "content": [{"type": "text", "text": "world"}]}}}
	var ev4 := {"type": "turn/end", "seq": 4, "data": {"turn": 1, "reason": {"kind": "completed"}}}
	fold1.ingest_history([ev1, ev2, ev3, ev4])
	_assert(fold1.nodes().size() == 2, "Fold1 has 2 nodes (user + asst)")
	_assert(fold1.seen_seq().has(1) and fold1.seen_seq().has(4), "Fold1 tracked seen seq 1..4")

	# 2. 测试 adopt 继承 seen_seq
	var fold2 := ConversationFold.new()
	fold2.adopt(fold1.nodes(), fold1.seen_seq())
	_assert(fold2.nodes().size() == 2, "Fold2 adopted 2 nodes")
	_assert(fold2.seen_seq().has(1) and fold2.seen_seq().has(4), "Fold2 retained seen_seq after adopt")

	# 3. 再次 ingest 相同 seq 的事件，应被忽略（防重复）
	var count_before := fold2.nodes().size()
	fold2.ingest(ev1)
	fold2.ingest(ev3)
	_assert(fold2.nodes().size() == count_before, "Re-ingesting duplicate seq does not add nodes")

	# 4. 测试 ChatList.set_nodes 幂等与 _is_same_nodes
	var chat := ChatList.new()
	chat.size = Vector2(800, 600)
	add_child(chat)
	await get_tree().process_frame

	chat.set_nodes(fold1.nodes(), fold1.seen_seq())
	await get_tree().process_frame
	var nodes_after_first := chat._nodes.size()
	_assert(nodes_after_first == 2, "ChatList populated 2 nodes")

	# 再次传入相同的 nodes，应当触发 _is_same_nodes 跳过重绘
	chat.set_nodes(fold1.nodes(), fold1.seen_seq())
	await get_tree().process_frame
	_assert(chat._nodes.size() == 2, "ChatList remains 2 nodes after redundant set_nodes")

	# 5. 模拟快速切换会话 A -> B -> A
	var fold_b := ConversationFold.new()
	var ev_b1 := {"type": "user/message", "seq": 10, "data": {"text": "query B", "turn": 1, "step": 1}}
	fold_b.ingest_history([ev_b1])

	# 切到 B
	chat.clear()
	_assert(chat.is_empty(), "ChatList is empty after clear()")
	chat.set_nodes(fold_b.nodes(), fold_b.seen_seq())
	await get_tree().process_frame
	_assert(chat._nodes.size() == 1, "ChatList has 1 node for session B")

	# 切回 A
	chat.clear()
	chat.set_nodes(fold1.nodes(), fold1.seen_seq())
	await get_tree().process_frame
	_assert(chat._nodes.size() == 2, "ChatList restored 2 nodes for session A")
	_assert(str(chat._nodes[0].get("id", "")).begins_with("user:"), "Node 0 is user message")
	_assert(str(chat._nodes[1].get("id", "")).begins_with("asst:"), "Node 1 is assistant message")

	# 验证结果
	print("=== PROBE RESULT: %d passed, %d failed ===" % [_passed, _failed])
	if _failed > 0:
		get_tree().quit(1)
	else:
		get_tree().quit(0)
