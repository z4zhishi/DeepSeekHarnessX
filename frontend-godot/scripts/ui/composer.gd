extends PanelContainer
class_name ComposerBar

## Phase 3 迁移契约（engine 化，W10-c 已切换）：composer 的结构由
## res://documents/composer_doc.gd 单一来源描述——spec() 是静态结构契约，
## build_mount_doc() 产出可被 DshUIReconciler 直接消费的挂载 AST。_ready 先
## 弃用 composer.tscn 静态子树（磁盘上场景文件原样保留作 legacy fallback），
## 再通过 DshUIReconciler + 注册 factory 把挂载文档实体化为唯一渲染
## （hero.gd 同款模式），行为层仅保留：按 key 解析成员、%X unique-name
## 重注册、槽位可见性、全部 DshTokens 着色与信号/键位行为。

signal prompt_submitted(text: String, attachments: Array)
signal stop_requested
signal command_submitted(line: String)
signal model_selected(id: String)
signal access_mode_requested(preset: String)
signal effort_changed(effort: String)
signal reject_all_toggled(enabled: bool)
signal chrome_customize_requested

const ACCESS_PRESETS: PackedStringArray = ["default", "accept-edits", "plan", "auto", "allow-all"]
const EFFORTS: PackedStringArray = ["high", "low", "max", "off"]
const REJECT_PATH := "user://approval_auto_reject.txt"
const ChromeLayoutScript := preload("res://scripts/ui/chrome/layout.gd")
const ChromeHostScript := preload("res://scripts/ui/chrome/host.gd")
const ChromeCatalogScript := preload("res://scripts/ui/chrome/catalog.gd")
# 跨文件类型走 preload 常量而非全局 class_name（hero.gd 同款惯例）；
# _doc 已在文件尾声明（build_doc() 静态回读），engine 挂载沿用同一 _doc 常量。
const DshReconT := preload("res://engine/reconciler.gd")

## 行为层成员 -> 挂载文档 key（documents/composer_doc.gd 的 SCENE_UNIQUE 同名
## 契约：scene 名 = composer.tscn unique_name_in_owner 名，%X 由此重建）。
const _MEMBER_DOCS := {
	"gen_status": "GenStatus",
	"queue_rail": "QueueRail",
	"attach_rail": "AttachRail",
	"cmd_palette": "CmdPalette",
	"prompt": "Prompt",
	"action_row": "Toolbar",
	"left_chrome": "LeftChrome",
	"access_chip": "AccessBtn",
	"reject_all": "RejectAllBtn",
	"action_spacer": "ActionSpacer",
	"right_chrome": "RightChrome",
	"model_effort": "ModelEffortBtn",
	"overflow_button": "OverflowBtn",
	"cmd_button": "CmdBtn",
	"model_picker": "ModelPicker",
	"attach_button": "AttachBtn",
	"attach_icon": "AttachIcon",
	"send_button": "SendBtn",
	"send_icon": "SendIcon",
}

# --- 挂载树成员（原 @onready 场景查找，W10-c 后由 _resolve_members 依据
# 文档 key 从 engine 实体化树回填；_ready 先挂载后解析，规避 @onready 时序）。
var _gen: Label = null
var _queue: HBoxContainer = null
var _rail: HBoxContainer = null
var _prompt: TextEdit = null
var _cmd: Button = null
var _access: Button = null
var _models: OptionButton = null
var _attach: Button = null
var _attach_icon: TextureRect = null
var _send: Button = null
var _send_icon: TextureRect = null
var _left_chrome: HBoxContainer = null
var _right_chrome: HBoxContainer = null
var _model_effort: Button = null
var _overflow: Button = null
var _reject: Button = null

var _recon = null
var _mounted_root: Control = null

var _generating := false
var _enabled := true
var _attachments: Array[String] = []
var _file_dialog: DshFilePicker = null
var _commands: Array = []
var _cmd_list: ItemList = null
var _syncing_models := false
var _access_i := 0
# Ask (default) / Edit (accept-edits) are first-class in a compact popover;
# Plan / Auto / Allow-all sit under the "more" group.
var _access_pop: PanelContainer = null
var _access_choice_btns: Array[Button] = []
var _access_group_common: Label = null
var _access_group_more: Label = null
var _effort := "high"
var _reject_all := false
var _chrome_catalog = null
var _chrome_layout = null
var _chrome_host = null
var _model_pop: PanelContainer = null
var _overflow_tray: PanelContainer = null
var _overflow_box: VBoxContainer = null
var _model_list: ItemList = null
var _effort_btns: Array[Button] = []

func _ready() -> void:
	size_flags_horizontal = Control.SIZE_SHRINK_CENTER
	# W10-c：场景子树弃用（磁盘 tscn 原样保留作 fallback），engine 文档是唯一渲染；
	# 必须先弃旧树再挂载，避免 tscn 的 %X 与 engine 重建的同名 %X 抢注册。
	_discard_legacy_scene_nodes()
	_recon = DshReconT.new()  # _init 内注册 builtin + interactive 类型
	_setup_chrome()
	_reapply_doc()
	_load_reject()
	var seat := get_parent() as Control
	if seat:
		seat.resized.connect(_cap_width)
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): _apply_strings())
	get_viewport().files_dropped.connect(_on_files_dropped)
	# 应用内附件选择器：常驻预实例化（非原生对话框），首点零冷启动；
	# 最近记录记入 "attachments" bucket，下次打开自动回到上次目录。
	_file_dialog = DshFilePicker.new()
	_file_dialog.bucket = "attachments"
	_file_dialog.files_selected.connect(_on_files_picked)
	_file_dialog.file_selected.connect(func(p: String): _on_files_picked(PackedStringArray([p])))
	add_child(_file_dialog)
	apply_tokens()
	_apply_strings()
	_grow()
	_refresh_send_state()
	call_deferred("_cap_width")


# --- W10-c engine 挂载（hero.gd 同款架构） ------------------------------------

## 丢弃 composer.tscn 遗留的静态子节点（VBox/GenStatus/QueueRail/…/SendIcon）：
## 挂载文档树现在是唯一渲染内容；磁盘上的 composer.tscn 保持原样作 fallback。
func _discard_legacy_scene_nodes() -> void:
	for child in get_children():
		remove_child(child)
		child.free()


## 单次挂载路径（结构唯一入口）：options（i18n placeholder + chrome 槽位 ids）
## -> _doc.build_mount_doc() -> reconciler 差分挂载/复用 -> 按 key 重解析成员
## -> 槽位可见性 -> 行为层着色。首挂整树；options 不变时全节点按 key 复用
## （reconciler 报 patched == 0，实例身份不变）。
func _reapply_doc() -> void:
	if _recon == null:
		return
	var doc: Dictionary = _doc.build_mount_doc(_mount_options())
	_recon.update(self, doc)
	_mounted_root = null
	if has_meta(DshReconT.META_ROOT):
		_mounted_root = get_meta(DshReconT.META_ROOT) as Control
	if _mounted_root == null or not is_instance_valid(_mounted_root):
		return
	var by_key := {}
	_collect_mounted(_mounted_root, by_key)
	_resolve_members(by_key)
	_apply_chrome_visibility(by_key)
	apply_tokens()


## 挂载 options：prompt 占位（同一 i18n helper）+ chrome 槽位 id 序列。
## 槽位空/无 layout 时不落该键，build_mount_doc 取其出厂布局。
func _mount_options() -> Dictionary:
	var options: Dictionary = {
		"placeholder": _t("chat.placeholder", _t("chat.messageAgent", "Message the agent")),
	}
	if _chrome_layout != null:
		var left: Array = []
		for v in _chrome_layout.widgets_for("composer.left"):
			left.append(str(v))
		var right: Array = []
		for v in _chrome_layout.widgets_for("composer.right"):
			right.append(str(v))
		options["left"] = left
		options["right"] = right
	return options


## 深度优先收集挂载树（含根）上 reconciler 落的 META_KEY -> 节点映射。
func _collect_mounted(node: Control, out: Dictionary) -> void:
	if node == null or not is_instance_valid(node):
		return
	var k := str(node.get_meta(DshReconT.META_KEY, ""))
	if k != "":
		out[k] = node
	for child in node.get_children():
		if child is Control:
			_collect_mounted(child as Control, out)


## %X unique-name 契约重建（W10-c 验收路径）：场景子树已弃用，engine 节点在
## 运行期补 owner = 本组件 + unique_name_in_owner，使探针/app 侧的
## get_node("%X") 相对 composer 实例照常解析。顺序为 name -> owner -> flag，
## 在两种注册实现下（set_owner 侧与 unique 标志侧）都落到同一映射。
func _register_unique(node: Control, doc_key: String) -> void:
	if node == null or not is_instance_valid(node):
		return
	var scene_unique := str(_MEMBER_DOCS.get(doc_key, ""))
	if scene_unique == "":
		return
	if node.name != scene_unique:
		node.name = scene_unique
	if node.owner != self:
		node.owner = self
	if not node.unique_name_in_owner:
		node.unique_name_in_owner = true


## 成员解析 + 首绑/换绑：按文档 key 取回实体化节点，映射回既有成员变量；
## 仅当实例更换（首挂或跨槽位 remount）时重连信号并落初始运行态——
## id 不变时这些成员原样保留（reconciler 复用契约）。
func _resolve_members(by_key: Dictionary) -> void:
	var gen := by_key.get("gen_status") as Label
	if gen != null and gen != _gen:
		_gen = gen
		_register_unique(_gen, "gen_status")
		_gen.visible = _generating
	var queue := by_key.get("queue_rail") as HBoxContainer
	if queue != null and queue != _queue:
		_queue = queue
		_register_unique(_queue, "queue_rail")
		_queue.visible = false
	var rail := by_key.get("attach_rail") as HBoxContainer
	if rail != null and rail != _rail:
		_rail = rail
		_register_unique(_rail, "attach_rail")
		_rail.visible = false
	var pal := by_key.get("cmd_palette") as ItemList
	if pal != null and pal != _cmd_list:
		_cmd_list = pal
		_register_unique(_cmd_list, "cmd_palette")
		_bind_cmd_palette()
	var prompt := by_key.get("prompt") as TextEdit
	if prompt != null and prompt != _prompt:
		_prompt = prompt
		_register_unique(_prompt, "prompt")
		_prompt.wrap_mode = TextEdit.LINE_WRAPPING_BOUNDARY
		if _prompt.get("scroll_fit_content_height") != null:
			_prompt.scroll_fit_content_height = true
		_prompt.caret_blink = true
		_prompt.resized.connect(_on_prompt_resized)
		_prompt.text_changed.connect(_on_text_changed)
	var access := by_key.get("access_chip") as Button
	if access != null and access != _access:
		_access = access
		_register_unique(_access, "access_chip")
		_access.pressed.connect(_on_access_pressed)
	var reject := by_key.get("reject_all") as Button
	if reject != null and reject != _reject:
		_reject = reject
		_register_unique(_reject, "reject_all")
		_reject.toggle_mode = true
		_reject.set_pressed_no_signal(_reject_all)
		_reject.toggled.connect(_on_reject_toggled)
	var left := by_key.get("left_chrome") as HBoxContainer
	if left != null and left != _left_chrome:
		_left_chrome = left
		_register_unique(_left_chrome, "left_chrome")
	var right := by_key.get("right_chrome") as HBoxContainer
	if right != null and right != _right_chrome:
		_right_chrome = right
		_register_unique(_right_chrome, "right_chrome")
	var model_effort := by_key.get("model_effort") as Button
	if model_effort != null and model_effort != _model_effort:
		_model_effort = model_effort
		_register_unique(_model_effort, "model_effort")
		_model_effort.pressed.connect(_toggle_model_pop)
	var overflow := by_key.get("overflow_button") as Button
	if overflow != null and overflow != _overflow:
		_overflow = overflow
		_register_unique(_overflow, "overflow_button")
		_overflow.pressed.connect(_toggle_overflow)
	var cmd := by_key.get("cmd_button") as Button
	if cmd != null and cmd != _cmd:
		_cmd = cmd
		_register_unique(_cmd, "cmd_button")
		_cmd.pressed.connect(_on_cmd_pressed)
	var models := by_key.get("model_picker") as OptionButton
	if models != null and models != _models:
		_models = models
		_register_unique(_models, "model_picker")
		_models.item_selected.connect(_on_model_item)
	var attach := by_key.get("attach_button") as Button
	if attach != null and attach != _attach:
		_attach = attach
		_register_unique(_attach, "attach_button")
		_attach.pressed.connect(_open_picker)
	var attach_icon := by_key.get("attach_icon") as TextureRect
	if attach_icon != null and attach_icon != _attach_icon:
		_attach_icon = attach_icon
		_register_unique(_attach_icon, "attach_icon")
	var send := by_key.get("send_button") as Button
	if send != null and send != _send:
		_send = send
		_register_unique(_send, "send_button")
		_send.pressed.connect(_on_send_pressed)
	var send_icon := by_key.get("send_icon") as TextureRect
	if send_icon != null and send_icon != _send_icon:
		_send_icon = send_icon
		_register_unique(_send_icon, "send_icon")


## 槽位可见性（行为层持有；文档对槽内 widget 不落 visible，见
## composer_doc.gd 行为层约束注释）：6 个 chrome widget 的 visible =
## 是否出现在 composer.left/right 槽位；落 overflow 槽或槽外一律隐藏
## （默认布局即 CmdBtn 隐藏，与场景时代一致）。model_picker 不在此列
## ——文档对其落有 visible:false（永隐藏，set_models 只启停）。
func _apply_chrome_visibility(by_key: Dictionary) -> void:
	if _chrome_layout == null:
		return
	var slotted := {}
	for slot in ["composer.left", "composer.right"]:
		for id in _chrome_layout.widgets_for(slot):
			slotted[str(id)] = true
	for wid in _doc.WIDGET_KEYS:
		var widget_id := str(wid)
		var node := by_key.get(String(_doc.WIDGET_KEYS[widget_id])) as Control
		if node == null:
			continue
		var want_visible := slotted.has(widget_id)
		if node.visible != want_visible:
			node.visible = want_visible


func _modal_blocks_keys() -> bool:
	var tree := get_tree()
	if tree == null:
		return false
	var n := tree.get_first_node_in_group("dsh_approval")
	return n != null and bool(n.get("visible"))


func _input(event: InputEvent) -> void:
	if _modal_blocks_keys():
		return
	if event is InputEventMouseButton and (event as InputEventMouseButton).pressed:
		_hide_pops_if_outside((event as InputEventMouseButton).global_position)
	if event is InputEventKey and event.pressed and not event.echo:
		var k := event as InputEventKey
		if k.keycode == KEY_ESCAPE:
			if _cmd_popup_visible():
				_hide_cmd_popup()
				get_viewport().set_input_as_handled()
				return
			if _hide_pops():
				get_viewport().set_input_as_handled()
				return
			if _generating:
				stop_requested.emit()
				get_viewport().set_input_as_handled()
			return
		if not _prompt.has_focus():
			return
		if _cmd_popup_visible() and (k.keycode == KEY_UP or k.keycode == KEY_DOWN):
			_move_cmd_sel(-1 if k.keycode == KEY_UP else 1)
			get_viewport().set_input_as_handled()
			return
		if k.keycode == KEY_TAB and _cmd_popup_visible():
			_apply_selected_cmd()
			get_viewport().set_input_as_handled()
			return
		if k.keycode == KEY_ENTER or k.keycode == KEY_KP_ENTER:
			if k.shift_pressed and not k.ctrl_pressed and not k.meta_pressed:
				return
			if str(DisplayServer.ime_get_text()) != "":
				return
			get_viewport().set_input_as_handled()
			if _cmd_popup_visible() and _cmd_list.get_selected_items().size() > 0:
				_apply_selected_cmd()
				return
			_submit()


func apply_tokens() -> void:
	# Apple 化（截图审出缺陷 5）：Composer 是主视觉锚点但仍要"贴地"——
	# 弱化为单层短距阴影（无漂浮感），依赖圆角与描边表达层级。
	var bg := DshTokens.bg_input()
	var outer := DshTokens.box(bg, DshTokens.RADIUS_COMPOSER, DshTokens.border_l2(), 1, Vector4(14, 10, 14, 8))
	outer.shadow_color = DshTokens.shadow_tinted()
	outer.shadow_size = 8
	outer.shadow_offset = Vector2(0, 3)
	add_theme_stylebox_override("panel", outer)
	if _prompt == null or _gen == null or _cmd == null:
		return  # 挂载失败的防御：不向空成员下笔，行为层随 _reapply_doc 重试
	_prompt.add_theme_color_override("font_color", DshTokens.text_primary())
	_prompt.add_theme_color_override("font_placeholder_color", DshTokens.text_tertiary())
	_gen.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_gen.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_cmd.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	_cmd.add_theme_color_override("font_color", DshTokens.text_secondary())
	DshIcons.apply(_attach_icon, "paperclip", 16.0)
	_refresh_send_icon()
	_paint_round(_cmd)
	_paint_access_chip()
	_paint_chip(_model_effort)
	_paint_chip(_overflow)
	_paint_reject()
	_paint_round(_attach)
	_paint_round(_send)
	_apply_strings()
	_refresh_model_effort_label()


func set_generating(generating: bool) -> void:
	_generating = generating
	_gen.visible = generating
	if generating:
		_gen.text = _t("chat.generating", "Deep diving...")
		_start_gen_pulse()
	else:
		_stop_gen_pulse()
		_clear_queue()
	_refresh_send_icon()
	_refresh_send_state()


## 生成中状态的三点呼吸动效（task #21 加载态）：Timer 循环重写文案尾部
## 省略号数（0..3），比纯静态文案更有“活着”的信号；主题禁动效时保持静态。
var _gen_dots := 0
var _gen_anim: Timer = null


func _start_gen_pulse() -> void:
	if _gen == null or not DshTokens.motion_enabled:
		return
	if _gen_anim == null:
		_gen_anim = Timer.new()
		_gen_anim.wait_time = 0.38
		_gen_anim.timeout.connect(_on_gen_tick)
		add_child(_gen_anim)
	_gen_anim.start()


func _stop_gen_pulse() -> void:
	_gen_dots = 0
	if _gen_anim != null and _gen_anim.is_inside_tree():
		_gen_anim.stop()


func _on_gen_tick() -> void:
	if _gen == null or not _generating:
		if _gen_anim != null:
			_gen_anim.stop()
		return
	_gen_dots = (_gen_dots + 1) % 4
	var base := _t("chat.generating", "Deep diving...").trim_suffix("...")
	_gen.text = "%s%s" % [base, ".".repeat(3 if _gen_dots == 0 else _gen_dots)]


func set_commands(commands: Array) -> void:
	_commands = commands
	_refresh_cmd_popup()


func set_enabled(enabled: bool) -> void:
	_enabled = enabled
	_prompt.editable = enabled
	_attach.disabled = not enabled
	_cmd.disabled = not enabled
	_access.disabled = not enabled
	_reject.disabled = not enabled
	_model_effort.disabled = (not enabled) or _models.item_count == 0
	_overflow.disabled = not enabled
	_models.disabled = (not enabled) or _models.item_count == 0
	_refresh_send_state()


func set_models(models: Array, selected: String) -> void:
	_syncing_models = true
	_models.clear()
	var pick := 0
	for m in models:
		var id := ""
		var label := ""
		if m is Dictionary:
			id = str(m.get("id", ""))
			label = str(m.get("name", id))
		else:
			id = str(m)
			label = id
		if id == "":
			continue
		_models.add_item(label)
		var idx := _models.item_count - 1
		_models.set_item_metadata(idx, id)
		if id == selected:
			pick = idx
	if _models.item_count > 0:
		_models.select(pick)
		_models.disabled = not _enabled
	else:
		_models.disabled = true
	_syncing_models = false
	_refresh_model_tooltip()
	_refresh_model_effort_label()
	_rebuild_model_pop_list()


func is_generating() -> bool:
	return _generating


func set_effort(effort: String) -> void:
	var e := effort.strip_edges()
	if EFFORTS.find(e) < 0:
		return
	_effort = e
	_refresh_model_effort_label()
	_sync_effort_btns()


func current_effort() -> String:
	return _effort


func set_reject_all(enabled: bool) -> void:
	_reject_all = enabled
	if _reject != null:
		_reject.set_pressed_no_signal(enabled)
	_paint_reject()


func is_reject_all() -> bool:
	return _reject_all


func selected_model() -> String:
	if _models.item_count == 0:
		return ""
	var idx := _models.selected
	if idx < 0:
		return ""
	return str(_models.get_item_metadata(idx))


func grab_input_focus() -> void:
	_prompt.grab_focus()


func get_draft() -> String:
	return _prompt.text


func set_draft(text: String) -> void:
	_prompt.text = text
	_normalize_slash_prefix()  # 草稿预填路径的显式归一（确定性入口，幂等）
	_grow()
	_refresh_cmd_popup()
	_refresh_send_state()


func _on_send_pressed() -> void:
	if _generating:
		stop_requested.emit()
		return
	_submit()


func _on_cmd_pressed() -> void:
	if not _enabled:
		return
	_prompt.grab_focus()
	_normalize_slash_prefix()  # 、/／ 起手的既有草稿同样视为 "/" 意图
	if not _prompt.text.begins_with("/"):
		_prompt.text = "/" + _prompt.text
	_prompt.set_caret_line(0)
	_prompt.set_caret_column(_prompt.get_line(0).length())
	_refresh_cmd_popup()
	_grow()
	_refresh_send_state()


## 全局命令面板公共入口（Ctrl+P，app.gd 热键调用）：与命令按钮同一行为面
## ——聚焦输入、注入 "/" 前缀、弹出命令候选。候选过滤 / 键盘导航 / Esc
## 收起语义全部沿用 _refresh_cmd_popup 既有行为，不引入新状态；app 侧
## 不允许绕过此入口触碰 cmd_palette 私有节点。
func open_command_palette() -> void:
	_on_cmd_pressed()


func _on_access_pressed() -> void:
	if _access_pop != null and _access_pop.visible:
		_hide_access_pop()
		return
	_hide_model_pop()
	_hide_overflow()
	_ensure_access_pop()
	_sync_access_pop()
	_access_pop.visible = true
	_place_pop(_access_pop, _access)
	DshTokens.pop_in(_access_pop)
	DshTokens.slide_in_y(_access_pop, 8.0, DshTokens.MOTION_SNAP)


func _set_access(idx: int, emit_signal: bool) -> void:
	if idx < 0 or idx >= ACCESS_PRESETS.size():
		return
	_access_i = idx
	_access.text = _access_label(ACCESS_PRESETS[idx])
	_paint_access_chip()
	if emit_signal:
		access_mode_requested.emit(ACCESS_PRESETS[idx])


## set_access_mode syncs the dropdown to a preset returned by session.policy (or
## a /permission command), so a resumed session shows its true mode instead of
## resetting to "default" on every launch.
func set_access_mode(preset: String) -> void:
	var idx := ACCESS_PRESETS.find(preset)
	if idx < 0:
		return
	if _access_i == idx:
		_access.text = _access_label(preset)
		_paint_access_chip()
		return
	_set_access(idx, false)


func current_access_mode() -> String:
	if _access_i < 0 or _access_i >= ACCESS_PRESETS.size():
		return "default"
	return ACCESS_PRESETS[_access_i]


func _on_model_item(index: int) -> void:
	if _syncing_models or index < 0:
		return
	var id := str(_models.get_item_metadata(index))
	_refresh_model_tooltip()
	_refresh_model_effort_label()
	if id != "":
		model_selected.emit(id)


func _submit() -> void:
	if not _enabled:
		return
	var text := _prompt.text.strip_edges()
	if _generating:
		if text == "":
			return
		_prompt.text = ""
		_grow()
		_hide_cmd_popup()
		_add_queue_chip(text)
		_refresh_send_state()
		if text.begins_with("/"):
			command_submitted.emit(text)
		else:
			prompt_submitted.emit(text, [])
		return
	if text == "" and _attachments.is_empty():
		return
	var paths: Array = []
	for p in _attachments:
		paths.append(p)
	if not paths.is_empty():
		var bits: PackedStringArray = PackedStringArray()
		for p in paths:
			bits.append("@" + str(p))
		if text != "":
			text += "\n"
		text += " ".join(bits)
	_prompt.text = ""
	_clear_attachments()
	_grow()
	_hide_cmd_popup()
	_refresh_send_state()
	if text.begins_with("/"):
		command_submitted.emit(text)
	else:
		prompt_submitted.emit(text, paths)


func _on_text_changed() -> void:
	if not _normalizing_slash:
		_normalize_slash_prefix()
	_grow()
	_refresh_cmd_popup()
	_refresh_send_state()


func _on_prompt_resized() -> void:
	_grow()


## 命令补全列表行为绑定：引擎已按 cmd_palette key 实体化 ItemList（挂在栈内
## prompt 之前），不再新建第二个列表；这里只补运行态显隐、样式与信号。
func _bind_cmd_palette() -> void:
	var lst := _cmd_list
	lst.visible = false
	lst.item_clicked.connect(func(index: int, _pos: Variant = null, _btn: Variant = 0) -> void:
		lst.select(index)
		_apply_selected_cmd()
	)
	lst.item_activated.connect(func(_index: int) -> void:
		_apply_selected_cmd()
	)
	lst.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)


func _cmd_popup_visible() -> bool:
	return _cmd_list != null and _cmd_list.visible


func _hide_cmd_popup() -> void:
	if _cmd_list != null:
		_cmd_list.visible = false
		_cmd_list.clear()


# --- IME 全角斜杠前缀归一（中文优先产品的身份级修复，supremacy-plan §1） ------
# 为什么：中文 IME 无法直出半角 "/"，用户起手指令实际落在 顿号(U+3001) /
# 全角斜杠(U+FF0F) 键位（，(U+FF0C) 不是斜杠键位，故不参与归一）。这里在
# composer 的 draft 层对首字符做一次性原位改写为 "/"：命令面板开关、候选
# 匹配与提交语义全部沿用 "/" 既有行为（command_submitted 仍收到以 "/"
# 开头的行）。只归一首字符，不改写正文其余部分。

## 归一触发的首字符集合：仅斜杠等价键位（顿号、全角斜杠），其余全角标点一律不动。
const _IME_SLASH_PREFIXES: PackedStringArray = ["、", "／"]

## 归一改写会同步再触发一次 text_changed；闸内跳过重入（改写后首字符已是
## "/"，本来也幂等，闸只为省一次空跑并保证“一次编辑轮次至多改写一次”）。
var _normalizing_slash := false


## 共享归一器：draft 首字符 ∈ {、, ／} 时原位改写为半角 "/"（幂等）。
## text_changed / set_draft / 命令按钮三条草稿路径都会走到这里。等长单字符
## 替换不改变行数与其余字符的列号；但 TextEdit 赋值 text 会重置 caret，
## 故先取后还（caret 保位）。
func _normalize_slash_prefix() -> void:
	if _prompt == null or _prompt.text.is_empty():
		return
	if not _IME_SLASH_PREFIXES.has(_prompt.text.substr(0, 1)):
		return
	var line := _prompt.get_caret_line()
	var col := _prompt.get_caret_column()
	_normalizing_slash = true
	# 原位改写：等长单字符替换，行/列号不变。
	_prompt.text = "/" + _prompt.text.substr(1)
	_normalizing_slash = false
	_prompt.set_caret_line(line)
	_prompt.set_caret_column(col)


func _refresh_cmd_popup() -> void:
	if _cmd_list == null:
		return
	var raw := _prompt.text
	if not raw.begins_with("/") or _commands.is_empty():
		_hide_cmd_popup()
		return
	var rest := raw.substr(1)
	if rest.find(" ") >= 0 or rest.find("\n") >= 0:
		_hide_cmd_popup()
		return
	var needle := rest.strip_edges().to_lower()
	_cmd_list.clear()
	for c in _commands:
		var name := ""
		var desc := ""
		if c is Dictionary:
			name = str((c as Dictionary).get("name", (c as Dictionary).get("id", "")))
			desc = str((c as Dictionary).get("description", (c as Dictionary).get("desc", "")))
		else:
			name = str(c)
		if name.begins_with("/"):
			name = name.substr(1)
		if name == "":
			continue
		if needle != "" and not name.to_lower().begins_with(needle) and name.to_lower().find(needle) < 0:
			continue
		var label := "/" + name
		if desc != "":
			label += "  —  " + desc
		_cmd_list.add_item(label)
		_cmd_list.set_item_metadata(_cmd_list.item_count - 1, name)
	if _cmd_list.item_count == 0:
		_hide_cmd_popup()
		return
	_cmd_list.visible = true
	_cmd_list.select(0)
	_cmd_list.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(4, 4, 4, 4)
	))


func _move_cmd_sel(delta: int) -> void:
	if _cmd_list == null or _cmd_list.item_count == 0:
		return
	var sel := _cmd_list.get_selected_items()
	var idx := sel[0] if not sel.is_empty() else 0
	idx = wrapi(idx + delta, 0, _cmd_list.item_count)
	_cmd_list.select(idx)
	_cmd_list.ensure_current_is_visible()


func _apply_selected_cmd() -> void:
	if _cmd_list == null:
		return
	var sel := _cmd_list.get_selected_items()
	if sel.is_empty():
		_hide_cmd_popup()
		return
	var name := str(_cmd_list.get_item_metadata(sel[0]))
	_prompt.text = "/" + name + " "
	_prompt.set_caret_line(_prompt.get_line_count() - 1)
	_prompt.set_caret_column(_prompt.get_line(_prompt.get_caret_line()).length())
	_hide_cmd_popup()
	_grow()
	_refresh_send_state()


func _grow() -> void:
	var logical_lines := maxi(_prompt.get_line_count(), 1)
	var wrap_width := maxi(int(_prompt.size.x), 1)
	var visual_lines := 0
	for i in logical_lines:
		var line_width := maxi(_prompt.get_line_width(i), 1)
		visual_lines += maxi(1, ceili(float(line_width) / float(wrap_width)))
	visual_lines = maxi(visual_lines, logical_lines)
	_prompt.custom_minimum_size.y = clampf(float(visual_lines * DshTokens.FONT_BODY_LH) + 8.0, 44.0, 140.0)


func _cap_width() -> void:
	var seat := get_parent() as Control
	var avail := seat.size.x if seat else DshTokens.COMPOSER_MAX
	custom_minimum_size.x = minf(DshTokens.COMPOSER_MAX, maxf(0.0, avail))
	_grow()


func _refresh_send_icon() -> void:
	DshIcons.apply(_send_icon, "stop" if _generating else "send", 16.0)
	_send.tooltip_text = _t("chat.stopTooltip", "Stop (Esc)") if _generating else _t("chat.sendTooltip", "Send (Enter)")


func _refresh_send_state() -> void:
	if _generating:
		_send.disabled = false
		return
	if not _enabled:
		_send.disabled = true
		return
	_send.disabled = _prompt.text.strip_edges() == "" and _attachments.is_empty()


func _refresh_model_tooltip() -> void:
	if _models.item_count == 0 or _models.selected < 0:
		_models.tooltip_text = _t("common.model", "Select model")
		return
	var cur := _models.get_item_text(_models.selected)
	_models.tooltip_text = "Select model, current %s" % cur


func _access_label(preset: String) -> String:
	match preset:
		"default":
			return _t("chat.accessAsk", "审批")
		"accept-edits":
			return _t("chat.accessEdit", "编辑")
		"plan":
			return _t("chat.accessPlan", "Plan")
		"auto":
			return _t("chat.accessAuto", "Auto(小模型审核)")
		"allow-all":
			return _t("chat.accessFull", "全部放行")
		_:
			return _t("chat.accessWrite", "Workspace Write")


## Queued text is display-only: the message was already steered to the backend,
## so a "remove" affordance would mislead (nothing can actually be recalled).
func _add_queue_chip(text: String) -> void:
	_queue.visible = true
	var wrap := PanelContainer.new()
	wrap.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_PILL,
		DshTokens.border_l1(),
		1,
		Vector4(8, 2, 8, 2)
	))
	var lab := Label.new()
	lab.text = text if text.length() <= 36 else text.substr(0, 33) + "…"
	lab.tooltip_text = text
	lab.mouse_filter = Control.MOUSE_FILTER_PASS
	lab.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	lab.add_theme_color_override("font_color", DshTokens.text_secondary())
	wrap.add_child(lab)
	DshTokens.fade_in(wrap, DshTokens.MOTION_QUICK)
	_queue.add_child(wrap)


func _refresh_queue_visible() -> void:
	var n := 0
	for c in _queue.get_children():
		if not c.is_queued_for_deletion():
			n += 1
	_queue.visible = n > 0


func _clear_queue() -> void:
	for c in _queue.get_children():
		c.queue_free()
	_queue.visible = false


func _open_picker() -> void:
	_file_dialog.open({
		"mode": "files",
		"title": _t("chat.attach", "Attach files"),
		"ratio": 0.6,
	})


func _on_files_picked(paths: PackedStringArray) -> void:
	for p in paths:
		_add_attachment(p)


func _on_files_dropped(paths: PackedStringArray) -> void:
	if not _enabled:
		return
	for p in paths:
		_add_attachment(p)


func _add_attachment(path: String) -> void:
	if path == "" or _attachments.has(path):
		return
	_attachments.append(path)
	_rail.visible = true
	var chip := Button.new()
	chip.text = path.get_file()
	chip.tooltip_text = path
	chip.flat = true
	chip.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	chip.add_theme_color_override("font_color", DshTokens.text_secondary())
	chip.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.border_l1(), 1, Vector4(8, 2, 8, 2)))
	chip.pressed.connect(func(): _remove_attachment(path, chip))
	_rail.add_child(chip)
	_refresh_send_state()


func _remove_attachment(path: String, chip: Button) -> void:
	_attachments.erase(path)
	chip.queue_free()
	if _attachments.is_empty():
		_rail.visible = false
	_refresh_send_state()


func _clear_attachments() -> void:
	_attachments.clear()
	for c in _rail.get_children():
		c.queue_free()
	_rail.visible = false
	_refresh_send_state()


func _paint_round(btn: Button) -> void:
	# 视觉重设计（task #20）：圆形图标钮——透明态、hover 轻投影 pill 抬亮、
	# pressed 语义下压；不再用 layer3 硬底。
	var pad := Vector4(6, 6, 6, 6)
	btn.add_theme_stylebox_override("normal", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	var hover_box := DshTokens.box(DshTokens.hover_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad)
	btn.add_theme_stylebox_override("hover", hover_box)
	btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, pad))
	btn.flat = true


func _paint_chip(btn: Button) -> void:
	var pad := Vector4(8, 4, 8, 4)
	btn.add_theme_stylebox_override("normal", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.hover_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, pad))
	btn.flat = true
	btn.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	# D1 修复：clip_text 打开时 Godot 将文本宽度从 Button 最小尺寸中剔除
	# （core get_minimum_size_for_text_and_icon），挂载文档又不落 min_width，
	# chip 会被压成 8~16px、内容矩形 0 宽 —— 文字完全裁没。文字 chip 必须
	# 按文案自适应宽度，故在此关闭裁剪。
	btn.clip_text = false


func _paint_access_chip() -> void:
	if _access == null:
		return
	var pad := Vector4(8, 4, 8, 4)
	var edit := current_access_mode() == "accept-edits"
	var bg := DshTokens.accent_soft() if edit else Color(0, 0, 0, 0)
	_access.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	_access.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.hover_layer() if not edit else bg, DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	_access.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	_access.add_theme_stylebox_override("focus", DshTokens.box(bg, DshTokens.RADIUS_PILL, DshTokens.accent(), 1, pad))
	_access.add_theme_color_override("font_color", DshTokens.accent() if edit else DshTokens.text_secondary())
	_access.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_access.flat = true
	_access.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	# D1 修复：clip_text 文字 chip 必须按文案自适应（理由同 _paint_chip 注释）。
	_access.clip_text = false


func _apply_strings() -> void:
	_prompt.placeholder_text = _t("chat.placeholder", _t("chat.messageAgent", "Message the agent"))
	_attach.tooltip_text = _t("chat.attach", "Attach files (@)")
	_cmd.tooltip_text = _t("chat.commands", "Commands")
	_access.tooltip_text = _t("chat.approval", "审批等级")
	_access.text = _access_label(ACCESS_PRESETS[_access_i])
	_paint_access_chip()
	_overflow.tooltip_text = _t("chat.more", "更多")
	_reject.tooltip_text = _t("chat.rejectAllHint", "开启后新的权限请求会自动拒绝，不打断当前任务")
	_model_effort.tooltip_text = _t("chat.modelEffort", "模型与思考等级")
	_gen.text = _t("chat.generating", "Deep diving...")
	_gen.tooltip_text = _t("chat.steerHint", "Ctrl+Enter to steer while generating")
	_refresh_send_icon()
	_refresh_model_tooltip()
	_refresh_model_effort_label()
	_paint_reject()
	_sync_access_pop()
	if _overflow_tray != null:
		_rebuild_overflow_tray()


func _setup_chrome() -> void:
	if _chrome_catalog == null:
		_chrome_catalog = ChromeCatalogScript.new()
	if _chrome_layout == null:
		_chrome_layout = ChromeLayoutScript.new(_chrome_catalog)
	_chrome_layout.load_layout()
	if _chrome_host == null:
		_chrome_host = ChromeHostScript.new()
		_chrome_host.register("approval", func() -> Control: return _access)
		_chrome_host.register("reject_all", func() -> Control: return _reject)
		_chrome_host.register("model_effort", func() -> Control: return _model_effort)
		_chrome_host.register("attach", func() -> Control: return _attach)
		_chrome_host.register("send", func() -> Control: return _send)
		_chrome_host.register("commands", func() -> Control: return _cmd)


func reload_chrome() -> void:
	if _chrome_catalog == null or _chrome_layout == null or _chrome_host == null:
		_setup_chrome()
		_reapply_doc()
		return
	if _chrome_layout.has_method("reload_from_disk"):
		_chrome_layout.reload_from_disk()
	else:
		_chrome_layout.load_layout()
	_reapply_doc()
	if _overflow_tray != null:
		_overflow_tray.visible = false
		_rebuild_overflow_tray()


func _load_reject() -> void:
	_reject_all = false
	if FileAccess.file_exists(REJECT_PATH):
		var f := FileAccess.open(REJECT_PATH, FileAccess.READ)
		if f != null:
			_reject_all = f.get_as_text().strip_edges() == "1"
	if _reject != null:
		_reject.set_pressed_no_signal(_reject_all)
	_paint_reject()


func _save_reject() -> void:
	var f := FileAccess.open(REJECT_PATH, FileAccess.WRITE)
	if f != null:
		f.store_string("1" if _reject_all else "0")


func _on_reject_toggled(pressed: bool) -> void:
	_reject_all = pressed
	_save_reject()
	_paint_reject()
	reject_all_toggled.emit(pressed)


func _paint_reject() -> void:
	if _reject == null:
		return
	var on := _reject_all
	_reject.text = _t("chat.rejectAllOn", "自动拒绝") if on else _t("chat.rejectAllOff", "需审批")
	var pad := Vector4(8, 4, 8, 4)
	var bg := DshTokens.warn() if on else Color(0, 0, 0, 0)
	if on:
		bg.a = 0.18 if DshTokens.is_dark() else 0.14
	_reject.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_PILL, DshTokens.warn() if on else Color(0, 0, 0, 0), 1 if on else 0, pad))
	_reject.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.hover_layer() if not on else bg, DshTokens.RADIUS_PILL, DshTokens.warn(), 1, pad))
	_reject.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, DshTokens.warn(), 1, pad))
	_reject.add_theme_color_override("font_color", DshTokens.warn() if on else DshTokens.text_secondary())
	_reject.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_reject.flat = true
	_reject.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	# D1 修复：clip_text 文字 chip 必须按文案自适应（理由同 _paint_chip 注释）。
	_reject.clip_text = false


func _effort_short() -> String:
	match _effort:
		"low":
			return _t("chat.effortLowShort", "低")
		"max":
			return _t("chat.effortMaxShort", "最大")
		"off":
			return _t("chat.effortOffShort", "关")
		_:
			return _t("chat.effortHighShort", "高")


func _compact_model_text(id: String) -> String:
	if id == "":
		return _t("common.model", "Model")
	var parts := id.split("-")
	if parts.size() >= 2:
		return "-".join(parts.slice(parts.size() - 2))
	return id


func _refresh_model_effort_label() -> void:
	if _model_effort == null:
		return
	var id := selected_model()
	if id == "" and _models.item_count > 0 and _models.selected >= 0:
		id = _models.get_item_text(_models.selected)
	_model_effort.text = "%s · %s" % [_compact_model_text(id), _effort_short()]
	_model_effort.tooltip_text = "%s — %s" % [_t("chat.modelEffort", "模型与思考等级"), _model_effort.text]
	_model_effort.disabled = (not _enabled) or _models.item_count == 0


func _toggle_model_pop() -> void:
	if _model_pop != null and _model_pop.visible:
		_hide_model_pop()
		return
	_hide_overflow()
	_hide_access_pop()
	_ensure_model_pop()
	_rebuild_model_pop_list()
	_sync_effort_btns()
	_model_pop.visible = true
	_place_pop(_model_pop, _model_effort)
	DshTokens.slide_in_y(_model_pop, 10.0, DshTokens.MOTION_SNAP)


func _toggle_overflow() -> void:
	if _overflow_tray != null and _overflow_tray.visible:
		_hide_overflow()
		return
	_hide_model_pop()
	_hide_access_pop()
	_ensure_overflow_tray()
	_rebuild_overflow_tray()
	_overflow_tray.visible = true
	_place_pop(_overflow_tray, _overflow)
	DshTokens.slide_in_y(_overflow_tray, 8.0, DshTokens.MOTION_SNAP)


func _hide_model_pop() -> void:
	if _model_pop != null:
		_model_pop.visible = false


func _hide_overflow() -> void:
	if _overflow_tray != null:
		_overflow_tray.visible = false


func _hide_access_pop() -> void:
	if _access_pop != null:
		_access_pop.visible = false


func _hide_pops() -> bool:
	var any := false
	if _model_pop != null and _model_pop.visible:
		_hide_model_pop()
		any = true
	if _overflow_tray != null and _overflow_tray.visible:
		_hide_overflow()
		any = true
	if _access_pop != null and _access_pop.visible:
		_hide_access_pop()
		any = true
	return any


func _hide_pops_if_outside(global_pos: Vector2) -> void:
	if _model_pop != null and _model_pop.visible:
		if not _hit(_model_pop, global_pos) and not _hit(_model_effort, global_pos):
			_hide_model_pop()
	if _overflow_tray != null and _overflow_tray.visible:
		if not _hit(_overflow_tray, global_pos) and not _hit(_overflow, global_pos):
			_hide_overflow()
	if _access_pop != null and _access_pop.visible:
		if not _hit(_access_pop, global_pos) and not _hit(_access, global_pos):
			_hide_access_pop()


func _hit(node: Control, global_pos: Vector2) -> bool:
	if node == null or not node.visible:
		return false
	return node.get_global_rect().has_point(global_pos)


func _place_pop(pop: Control, anchor: Control) -> void:
	if pop == null or anchor == null:
		return
	pop.reset_size()
	var g := anchor.get_global_rect()
	var size := pop.get_combined_minimum_size()
	if pop.size.x > size.x:
		size.x = pop.size.x
	if pop.size.y > size.y:
		size.y = pop.size.y
	var x := g.position.x + g.size.x - size.x
	var y := g.position.y - size.y - 8.0
	if x < 8.0:
		x = 8.0
	if y < 8.0:
		y = g.position.y + g.size.y + 8.0
	pop.global_position = Vector2(x, y)


func _ensure_model_pop() -> void:
	if _model_pop != null:
		return
	_model_pop = PanelContainer.new()
	_model_pop.visible = false
	_model_pop.top_level = true
	_model_pop.custom_minimum_size = Vector2(280, 220)
	_model_pop.add_theme_stylebox_override("panel", DshTokens.elevated(
		DshTokens.bg_layer1(), DshTokens.RADIUS_LG, Vector4(14, 12, 14, 12), 3
	))
	add_child(_model_pop)
	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 10)
	_model_pop.add_child(box)
	var title := Label.new()
	title.text = _t("chat.modelEffort", "模型与思考等级")
	title.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	title.add_theme_color_override("font_color", DshTokens.text_primary())
	box.add_child(title)
	var effort_label := Label.new()
	effort_label.text = _t("chat.effort", "思考等级")
	effort_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	effort_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
	box.add_child(effort_label)
	var effort_row := HBoxContainer.new()
	effort_row.add_theme_constant_override("separation", 6)
	box.add_child(effort_row)
	_effort_btns.clear()
	for e in EFFORTS:
		var b := Button.new()
		b.flat = true
		b.text = _effort_label(e)
		b.pressed.connect(_on_effort_picked.bind(e))
		effort_row.add_child(b)
		_effort_btns.append(b)
		_paint_chip(b)
	_model_list = ItemList.new()
	_model_list.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_model_list.custom_minimum_size = Vector2(0, 140)
	_model_list.item_selected.connect(_on_pop_model)
	box.add_child(_model_list)


func _effort_label(e: String) -> String:
	match e:
		"low":
			return _t("app.effortLow", "Effort: low")
		"max":
			return _t("app.effortMax", "Effort: max")
		"off":
			return _t("app.effortOff", "Effort: off")
		_:
			return _t("app.effortHigh", "Effort: high")


func _on_effort_picked(e: String) -> void:
	if EFFORTS.find(e) < 0:
		return
	_effort = e
	_sync_effort_btns()
	_refresh_model_effort_label()
	effort_changed.emit(e)


func _sync_effort_btns() -> void:
	for i in _effort_btns.size():
		var b := _effort_btns[i]
		var on := EFFORTS[i] == _effort
		b.add_theme_color_override("font_color", DshTokens.accent() if on else DshTokens.text_secondary())
		var pad := Vector4(8, 4, 8, 4)
		var bg := DshTokens.accent_soft() if on else Color(0, 0, 0, 0)
		b.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))


func _rebuild_model_pop_list() -> void:
	if _model_list == null:
		return
	_model_list.clear()
	var pick := 0
	var cur := selected_model()
	for i in _models.item_count:
		var id := str(_models.get_item_metadata(i))
		var label := _models.get_item_text(i)
		_model_list.add_item(label if label != "" else id)
		_model_list.set_item_metadata(_model_list.item_count - 1, id)
		if id == cur:
			pick = i
	if _model_list.item_count > 0:
		_model_list.select(pick)


func _on_pop_model(index: int) -> void:
	if _syncing_models or index < 0 or index >= _model_list.item_count:
		return
	var id := str(_model_list.get_item_metadata(index))
	if id == "":
		return
	for i in _models.item_count:
		if str(_models.get_item_metadata(i)) == id:
			_syncing_models = true
			_models.select(i)
			_syncing_models = false
			break
	_refresh_model_effort_label()
	model_selected.emit(id)


func _ensure_access_pop() -> void:
	if _access_pop != null:
		return
	_access_pop = PanelContainer.new()
	_access_pop.visible = false
	_access_pop.top_level = true
	_access_pop.custom_minimum_size = Vector2(220, 0)
	_access_pop.add_theme_stylebox_override("panel", DshTokens.elevated(
		DshTokens.bg_layer1(), DshTokens.RADIUS_LG, Vector4(12, 10, 12, 10), 2
	))
	add_child(_access_pop)
	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 8)
	_access_pop.add_child(box)
	_access_group_common = Label.new()
	_access_group_common.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_access_group_common.add_theme_color_override("font_color", DshTokens.text_tertiary())
	box.add_child(_access_group_common)
	var common_row := HBoxContainer.new()
	common_row.add_theme_constant_override("separation", 6)
	box.add_child(common_row)
	_access_choice_btns.clear()
	_add_access_choice(common_row, "default", true)
	_add_access_choice(common_row, "accept-edits", true)
	_access_group_more = Label.new()
	_access_group_more.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_access_group_more.add_theme_color_override("font_color", DshTokens.text_tertiary())
	box.add_child(_access_group_more)
	var more := VBoxContainer.new()
	more.add_theme_constant_override("separation", 2)
	box.add_child(more)
	for preset in ACCESS_PRESETS:
		if preset == "default" or preset == "accept-edits":
			continue
		_add_access_choice(more, preset, false)


func _add_access_choice(parent: Control, preset: String, expand: bool) -> void:
	var b := Button.new()
	b.flat = true
	b.set_meta("preset", preset)
	if expand:
		b.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	else:
		b.alignment = HORIZONTAL_ALIGNMENT_LEFT
		b.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	b.pressed.connect(_on_access_picked.bind(preset))
	parent.add_child(b)
	_access_choice_btns.append(b)


func _on_access_picked(preset: String) -> void:
	var idx := ACCESS_PRESETS.find(preset)
	if idx < 0:
		return
	_set_access(idx, true)
	_hide_access_pop()


func _sync_access_pop() -> void:
	if _access_pop == null:
		return
	if _access_group_common != null:
		_access_group_common.text = _t("chat.accessGroupCommon", "常用")
	if _access_group_more != null:
		_access_group_more.text = _t("chat.accessGroupMore", "其他")
	var cur := current_access_mode()
	for b in _access_choice_btns:
		if not b.has_meta("preset"):
			continue
		var preset := str(b.get_meta("preset"))
		b.text = _access_label(preset)
		_paint_access_choice(b, preset == cur, preset)


func _paint_access_choice(btn: Button, selected: bool, preset: String) -> void:
	var pad := Vector4(10, 6, 10, 6)
	var edit := preset == "accept-edits"
	var bg := Color(0, 0, 0, 0)
	if selected and edit:
		bg = DshTokens.accent_soft()
	elif selected:
		bg = DshTokens.hover_layer()
	btn.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.hover_layer() if not selected else bg, DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_color_override("font_color", DshTokens.accent() if selected else DshTokens.text_secondary())
	btn.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	btn.flat = true
	btn.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND


func _ensure_overflow_tray() -> void:
	if _overflow_tray != null:
		return
	_overflow_tray = PanelContainer.new()
	_overflow_tray.visible = false
	_overflow_tray.top_level = true
	_overflow_tray.custom_minimum_size = Vector2(200, 0)
	_overflow_tray.add_theme_stylebox_override("panel", DshTokens.elevated(
		DshTokens.bg_layer1(), DshTokens.RADIUS_LG, Vector4(10, 8, 10, 8), 2
	))
	add_child(_overflow_tray)
	_overflow_box = VBoxContainer.new()
	_overflow_box.add_theme_constant_override("separation", 4)
	_overflow_tray.add_child(_overflow_box)


func _rebuild_overflow_tray() -> void:
	if _overflow_box == null:
		return
	while _overflow_box.get_child_count() > 0:
		var child := _overflow_box.get_child(0)
		_overflow_box.remove_child(child)
		child.free()
	if _chrome_layout != null:
		var overflow_ids: PackedStringArray = _chrome_layout.widgets_for("composer.overflow")
		for id in overflow_ids:
			if str(id) == "commands":
				var b := Button.new()
				b.text = _t("chat.slash", "斜杠命令（/）")
				b.flat = true
				b.alignment = HORIZONTAL_ALIGNMENT_LEFT
				_paint_chip(b)
				b.pressed.connect(_on_overflow_commands)
				_overflow_box.add_child(b)
	var custom := Button.new()
	custom.text = _t("chrome.customize", "自定义工具栏")
	custom.flat = true
	custom.alignment = HORIZONTAL_ALIGNMENT_LEFT
	_paint_chip(custom)
	custom.pressed.connect(_on_chrome_customize)
	_overflow_box.add_child(custom)


func _on_overflow_commands() -> void:
	_hide_overflow()
	_on_cmd_pressed()


func _on_chrome_customize() -> void:
	_hide_overflow()
	chrome_customize_requested.emit()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v


# --- Phase 3 迁移契约（engine 化，文档先行） ----------------------------------
# 结构文档单一来源在 documents/composer_doc.gd（详见其文件头说明）。按约束本段
# 仅追加于文件尾：const 声明不影响既有行为，build_doc() 为纯静态回读——不改样式、
# 不碰节点树、不产生信号；真正的 engine 渲染切换待 Phase 3 验收后另行实施。
const _doc := preload("res://documents/composer_doc.gd")


## Phase 3 engine 迁移契约：返回 composer 的结构文档（DshUIDocument AST）。
## 与文档探针 tests/composer_doc_probe 对齐；spec() 为唯一权威，本方法仅回读。
func build_doc() -> Dictionary:
	return _doc.spec()
