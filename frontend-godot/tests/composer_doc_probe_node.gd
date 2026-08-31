extends Node

## Composer 文档契约探针（Phase 3 engine 化前置）。
## 纯静态：断言 documents/composer_doc.gd 的 spec() AST 结构与 key 稳定性，
## 并验证 composer.gd 追加的 build_doc() 回读一致。
## 不实例化到树、不触碰 @onready、不产生信号——对运行态零侵入。
## 运行：godot --headless --path frontend-godot --script res://tests/composer_doc_probe.gd
##
## 注意：composer.gd 依赖 autoload（DshI18n/DshTokens），--script 模式的
## wrapper _init（autoload 注册前）不能编译它——故本探针只在首个 idle 帧
## 之后用 load() 惰性拉起 composer 依赖（与 probe_gui_structure_node 的
## _ready 内 load app.tscn 同理），顶部仅保留无外部依赖的 composer_doc。

const Doc := preload("res://documents/composer_doc.gd")

## 任务要求的四个锚点节点：key -> [期望 type, 期望 meta.class, 期望 scene_unique]
const ANCHORS := {
	"prompt": ["text_input", "TextEdit", "Prompt"],
	"send_button": ["icon_button", "Button", "SendBtn"],
	"access_chip": ["chip", "Button", "AccessBtn"],
	"model_picker": ["dropdown", "OptionButton", "ModelPicker"],
}

var _passed := 0
var _failed := 0


func _ready() -> void:
	# 首帧 idle 前全局脚本类/autoload 已注册（probe_gui_structure 同款时序），
	# 之后才允许加载 composer.gd。
	await get_tree().process_frame
	var composer_gui: Variant = _load_composer_gui()
	var spec := _get_spec()
	if spec.is_empty():
		_failed += 1
		print("  [FAIL] spec() 返回空或非 Dictionary")
		print("COMPOSER_DOC_RESULT passed=%d failed=%d" % [_passed, _failed])
		get_tree().quit(1)
		return
	_run(spec, composer_gui)
	print("COMPOSER_DOC_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _run(doc: Dictionary, composer_gui: Variant) -> void:
	_check_doc_shell(doc)
	_check_required_nodes(doc)
	_check_hierarchy(doc)
	_check_keys(doc)
	_check_contract(doc, composer_gui)
	_check_determinism(doc)
	_check_build_doc(doc)


# --- 分节断言 ----------------------------------------------------------------

func _check_doc_shell(doc: Dictionary) -> void:
	for field in ["version", "kind", "component", "source", "root", "signals", "api", "behaviors", "planned_types"]:
		_assert(doc.has(field), "spec 顶层字段 %s 存在" % field)
	_assert(doc.get("source", {}) is Dictionary, "source 分节为 Dictionary")
	var source: Dictionary = doc.get("source", {}) if doc.get("source", {}) is Dictionary else {}
	_assert(str(source.get("scene", "")).ends_with("scenes/chrome/composer.tscn"), "source.scene 指向 composer.tscn")
	_assert(str(source.get("script", "")).ends_with("scripts/ui/composer.gd"), "source.script 指向 composer.gd")
	_assert(doc.get("root", {}) is Dictionary, "root 为 AST Dictionary")
	if not (doc.get("root", {}) is Dictionary):
		return
	var r := doc.get("root", {}) as Dictionary
	_assert(str(r.get("type", "")) == "composer", "root.type == composer")
	_assert(str(r.get("key", "")) == "composer", "root.key == composer")
	_assert(r.get("props", {}) is Dictionary and r.get("children", []) is Array, "root 带 props/children")
	var shape := _shape_errors(r)
	_assert(shape.is_empty(), "AST 形状（type/key/props/children）逐节点合法 %s" % [shape])


func _check_required_nodes(doc: Dictionary) -> void:
	var root := doc.get("root", {}) as Dictionary
	for key in ANCHORS.keys():
		var want: Array = ANCHORS[key]
		var node := Doc.find(root, str(key))
		if node.is_empty():
			_assert(false, "AST 含锚点节点 %s" % key)
			continue
		_assert(str(node.get("type", "")) == str(want[0]), "%s.type == %s" % [key, want[0]])
		var meta := _meta(node)
		_assert(str(meta.get("class", "")) == str(want[1]), "%s.meta.class == %s" % [key, want[1]])
		_assert(str(meta.get("scene_unique", "")) == str(want[2]),
			"%s.meta.scene_unique == %s（tscn unique name 绑定）" % [key, want[2]])


func _check_hierarchy(doc: Dictionary) -> void:
	var root := doc.get("root", {}) as Dictionary
	_assert(_child_keys(root) == ["composer_stack", "file_picker", "access_menu"],
		"root 子序 [stack, file_picker, access_menu]")
	var stack := Doc.find(root, "composer_stack")
	_assert(not stack.is_empty() and str(stack.get("type", "")) == "column", "composer_stack 为 column（VBox）")
	# 运行期 _build_cmd_list 把补全列表插到 prompt 之前，文档必须如实收录。
	_assert(_child_keys(stack) == ["gen_status", "queue_rail", "attach_rail", "cmd_palette", "prompt", "action_row"],
		"stack 子序与运行期树一致（含 cmd_palette 在 prompt 前）")
	var row := Doc.find(root, "action_row")
	_assert(_child_keys(row) == ["left_chrome", "action_spacer", "right_chrome"],
		"action_row 子序 left/spacer/right")
	var left := Doc.find(root, "left_chrome")
	_assert(_child_keys(left) == ["access_chip", "reject_all"],
		"left_chrome 审批等级 + 自动拒绝")
	var right := Doc.find(root, "right_chrome")
	_assert(_child_keys(right) == ["model_effort", "overflow_button", "cmd_button", "model_picker", "attach_button", "send_button"],
		"right_chrome 模型·effort / 溢出 / 附件 / 发送")
	var send := Doc.find(root, "send_button")
	_assert(_child_keys(send) == ["send_icon"], "send_button 内含 send_icon（状态切换 glyph）")
	var attach := Doc.find(root, "attach_button")
	_assert(_child_keys(attach) == ["attach_icon"], "attach_button 内含 attach_icon")


func _check_keys(doc: Dictionary) -> void:
	var root := doc.get("root", {}) as Dictionary
	var all := Doc.nodes(root)
	_assert(all.size() >= 12, "AST 展平含 %d 个节点" % all.size())
	var seen := {}
	var dup := ""
	for n in all:
		var k := str((n as Dictionary).get("key", ""))
		if k == "":
			continue
		if seen.has(k):
			dup = k
		seen[k] = true
	_assert(dup == "", "AST key 无重复（首个冲突: %s）" % dup)
	_assert(seen.size() >= 12, "非空 key 数量 %d" % seen.size())


func _check_contract(doc: Dictionary, composer_gui: Variant) -> void:
	var signals := doc.get("signals", {}) as Dictionary
	var want_signals := ["prompt_submitted", "stop_requested", "command_submitted", "model_selected", "access_mode_requested"]
	var ok_sig := true
	for s in want_signals:
		if not signals.has(s):
			ok_sig = false
	_assert(ok_sig, "signals 含五个对外信号（带参说明）")
	var api_ids: Array = []
	for entry in doc.get("api", []):
		if entry is Dictionary and (entry as Dictionary).has("method"):
			api_ids.append(str((entry as Dictionary)["method"]))
	var want_api := ["set_generating", "set_enabled", "set_models", "set_access_mode", "set_draft", "get_draft", "grab_input_focus"]
	var ok_api := true
	for m in want_api:
		if not api_ids.has(m):
			ok_api = false
	_assert(ok_api, "api 含七个外部调用契约方法")
	var behaviors: Variant = doc.get("behaviors", [])
	_assert(behaviors is Array and (behaviors as Array).size() >= 8, "behaviors 收录运行期行为清单")
	var chip := Doc.find(doc.get("root", {}) as Dictionary, "access_chip")
	var options: Variant = _meta(chip).get("options", [])
	if composer_gui is GDScript:
		_assert(options is Array and (options as Array).size() == (composer_gui as GDScript).ACCESS_PRESETS.size()
			and _array_match(options as Array, (composer_gui as GDScript).ACCESS_PRESETS),
			"access_chip.options 与 composer 常量 ACCESS_PRESETS 一致（无漂移）")
	else:
		_assert(false, "composer.gd 可编译（无法执行 ACCESS_PRESETS 漂移检查）")


func _check_determinism(doc: Dictionary) -> void:
	var again: Dictionary = Doc.spec()
	_assert(again == doc, "连续两次 spec() 深度相等（key/props 稳定）")
	var root := (again as Dictionary).get("root", {}) as Dictionary
	var stable := true
	for key in ANCHORS.keys():
		var node := Doc.find(root, str(key))
		if node.is_empty() or str(node.get("key", "")) != str(key):
			stable = false
	_assert(stable, "锚点 key 二次查找一致（key 稳定）")


func _check_build_doc(doc: Dictionary) -> void:
	var scn: PackedScene = load("res://scenes/chrome/composer.tscn")
	if scn == null or not (scn.can_instantiate()):
		_assert(false, "composer.tscn 可加载")
		return
	var bar := scn.instantiate()
	if bar == null or not bar.has_method("build_doc"):
		_assert(false, "composer 实例提供 build_doc()")
		if bar != null:
			bar.free()
		return
	var got: Variant = bar.call("build_doc")
	_assert(got == doc, "composer 场景实例 build_doc() == spec()（纯回读）")
	bar.free()


# --- 工具 --------------------------------------------------------------------

## composer.gd 的加载与可用性：--script 模式下 autoload 未就绪时编译会失败，
## 返回 null 让后续检查以显式 [FAIL] 呈现，而不是让探针崩溃或悄悄跳过。
func _load_composer_gui() -> Variant:
	var script: Variant = load("res://scripts/ui/composer.gd")
	if script is GDScript and (script as GDScript).can_instantiate():
		return script
	_failed += 1
	print("  [FAIL] composer.gd 编译/可用（autoload 依赖未注册或脚本错误）")
	return null


func _get_spec() -> Dictionary:
	var s: Variant = Doc.spec()
	if s is Dictionary and not (s as Dictionary).is_empty():
		return s as Dictionary
	return {}


func _child_keys(n: Dictionary) -> Array:
	var out: Array = []
	for c in n.get("children", []):
		if c is Dictionary:
			out.append(str((c as Dictionary).get("key", "")))
	return out


func _meta(n: Dictionary) -> Dictionary:
	if n.get("meta", {}) is Dictionary:
		return n.get("meta", {}) as Dictionary
	return {}


## DshUIDocument 契约的本地形状校验（不依赖 engine 全局类，保持探针独立）。
func _shape_errors(n: Dictionary) -> PackedStringArray:
	var errs := PackedStringArray()
	if str(n.get("type", "")).strip_edges() == "":
		errs.append("节点缺 type")
	if not n.has("key") or str(n.get("key", "")) == "":
		errs.append("节点 %s 缺 key" % str(n.get("type", "")))
	if not (n.get("props", {}) is Dictionary):
		errs.append("节点 %s props 非 Dictionary" % str(n.get("key", "")))
	if not (n.get("children", []) is Array):
		errs.append("节点 %s children 非 Array" % str(n.get("key", "")))
	for c in n.get("children", []):
		if c is Dictionary:
			errs.append_array(_shape_errors(c as Dictionary))
	return errs


func _array_match(a: Array, b: PackedStringArray) -> bool:
	if a.size() != b.size():
		return false
	for i in a.size():
		if str(a[i]) != str(b[i]):
			return false
	return true


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  [PASS] %s" % msg)
	else:
		_failed += 1
		print("  [FAIL] %s" % msg)