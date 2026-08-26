extends CanvasLayer
class_name ApprovalOverlay

## Modal approval card. Decisions are backend strings:
## allow_once | allow_all | deny | cancel.
##
## 操作质量契约（对标一线 CLI 权限流，界面原创）：
## - 全键盘可选：1/Y = 允许一次，2/N = 拒绝，3 = 本会话总是允许，Esc = 取消；
##   卡片打开即聚焦主按钮（Enter 直达，方向键换选）。
## - 状态自解释：顶部两行结构化摘要（等宽工具名 + 目标/命令首行），
##   完整 prompt 放可滚动详情区；按钮文本标注键位。
## - 终态必达：本地 60s 数字倒计时近似同步网关侧 approvalTimeout（网关为
##   权威时钟）；归零强制走 cancel 保证卡片必然关闭。
##   host/permission-resolved 下行帧经 resolve_remote() 关闭在途卡片，
##   不发本地决策（决策非本端产生）。
## - 键位门控：快捷键仅在对应决策项真实存在时生效，避免对 ask_user 等
##   自由选项卡片误触发。

signal decision_made(call_id: String, decision: String)

const APPROVAL_WINDOW_SEC := 60

## 决策 -> 按钮键位标注（仅标准决策有；自由选项按钮不标键位）。
const KEY_HINTS := {
	"allow_once": "1",
	"deny": "2",
	"allow_all": "3",
	"cancel": "Esc",
}

var _call_id: String = ""
var _raw_prompt := ""
var _raw_options: Array = []
var _remaining := APPROVAL_WINDOW_SEC
var _kbd_allow_once := false
var _kbd_deny := false
var _kbd_allow_all := false
var _backdrop: ColorRect
var _card: PanelContainer
var _title: Label
var _summary_box: VBoxContainer
var _tool_label: Label
var _target_label: Label
var _prompt: RichTextLabel
var _btn_row: HBoxContainer
var _icon: TextureRect
var _countdown: Label
var _tick: Timer


func _ready() -> void:
	layer = 24
	visible = false
	process_mode = Node.PROCESS_MODE_ALWAYS
	add_to_group("dsh_approval")
	_build()
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_on_locale_changed)


func _build() -> void:
	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.55)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	_backdrop.gui_input.connect(_on_backdrop_input)
	add_child(_backdrop)

	var center := CenterContainer.new()
	center.set_anchors_preset(Control.PRESET_FULL_RECT)
	center.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(center)

	_card = PanelContainer.new()
	_card.custom_minimum_size = Vector2(480, 260)
	center.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 12)
	_card.add_child(box)

	var title_row := HBoxContainer.new()
	title_row.add_theme_constant_override("separation", 8)
	box.add_child(title_row)

	_icon = TextureRect.new()
	_icon.custom_minimum_size = Vector2(20, 20)
	_icon.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	_icon.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	var tex := load("res://assets/icons/icon_warning.svg")
	if tex is Texture2D:
		_icon.texture = tex
	_icon.modulate = DshTokens.warn()
	title_row.add_child(_icon)

	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", 16)
	title_row.add_child(_title)

	# 剩余窗口数字倒计时（等宽字体防抖动），颜色随紧迫度分级。
	_countdown = Label.new()
	_countdown.add_theme_font_override("font", DshThemeBuilder.code_font())
	_countdown.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	title_row.add_child(_countdown)

	# 两行结构化摘要：工具名（等宽）+ 目标首行。解析失败时整块隐藏。
	_summary_box = VBoxContainer.new()
	_summary_box.add_theme_constant_override("separation", 2)
	_summary_box.visible = false
	box.add_child(_summary_box)

	_tool_label = Label.new()
	_tool_label.add_theme_font_override("font", DshThemeBuilder.code_font())
	_tool_label.add_theme_font_size_override("font_size", DshTokens.FONT_CODE)
	_tool_label.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	_tool_label.clip_text = true
	_summary_box.add_child(_tool_label)

	_target_label = Label.new()
	_target_label.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_target_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	_target_label.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	_target_label.clip_text = true
	_summary_box.add_child(_target_label)

	# 完整 prompt 详情：固定高度可滚动区，长文本不再撑爆卡片。
	_prompt = RichTextLabel.new()
	_prompt.bbcode_enabled = true
	_prompt.fit_content = false
	_prompt.scroll_active = true
	_prompt.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_prompt.custom_minimum_size = Vector2(0, 88)
	box.add_child(_prompt)

	_btn_row = HBoxContainer.new()
	_btn_row.add_theme_constant_override("separation", 10)
	_btn_row.alignment = BoxContainer.ALIGNMENT_END
	box.add_child(_btn_row)

	_tick = Timer.new()
	_tick.one_shot = false
	_tick.wait_time = 1.0
	_tick.timeout.connect(_on_tick)
	add_child(_tick)

	_apply_style()
	_title.text = _title_text()


func _apply_style() -> void:
	var card_bg := DshTokens.bg_layer1()
	card_bg.a = 0.98
	var card_box := DshTokens.box(card_bg, DshTokens.RADIUS_LG, DshTokens.warn(), 1, Vector4(20, 18, 20, 18))
	card_box.shadow_size = 0
	_card.add_theme_stylebox_override("panel", card_box)
	_title.add_theme_color_override("font_color", DshTokens.warn())
	_prompt.add_theme_color_override("default_color", DshTokens.text_primary())
	_icon.modulate = DshTokens.warn()
	_tool_label.add_theme_color_override("font_color", DshTokens.text_primary())
	_target_label.add_theme_color_override("font_color", DshTokens.text_secondary())
	_update_countdown_label()


func _title_text() -> String:
	return _t("approval.title", "安全授权确认", "Security Authorization Required")


func _t(key: String, zh: String, en: String) -> String:
	# i18n 词表由 autoload 持有且不在本改动白名单内；键缺失时按当前语言回退。
	var v := str(DshI18n.t(key))
	if v == key or v.strip_edges() == "":
		return zh if DshI18n.is_zh() else en
	return v


func _on_locale_changed(_loc: String) -> void:
	_title.text = _title_text()
	if visible:
		_rebuild_buttons(_raw_options)
		_render_prompt_summary(_raw_prompt)
		_apply_style()


func show_request(call_id: String, prompt: String, options: Array = []) -> void:
	_call_id = call_id
	_raw_prompt = prompt
	_raw_options = options.duplicate()
	_title.text = _title_text()
	_render_prompt_summary(prompt)
	_rebuild_buttons(options)
	_apply_style()
	_fit_card()
	# Instant show: fade-to-zero left an invisible CanvasLayer with
	# MOUSE_FILTER_STOP over the whole window, which the user reads as 卡死.
	if _backdrop != null:
		_backdrop.modulate.a = 1.0
	if _card != null:
		_card.modulate.a = 1.0
	visible = true
	_start_countdown()
	_focus_primary()


func hide_request() -> void:
	_stop_countdown()
	visible = false
	_call_id = ""


func cancel_open() -> void:
	_decide("cancel")


func _fit_card() -> void:
	if _card == null:
		return
	var vp := get_viewport().get_visible_rect().size
	var w := minf(560.0, maxf(320.0, vp.x - 48.0))
	var h := minf(320.0, maxf(220.0, vp.y - 96.0))
	_card.custom_minimum_size = Vector2(w, h)


## 关闭一张因外部原因已成终态的卡片（网关超时广播 / 其他端已决）。
## 命中返回 true；不发 decision_made —— 该决策不是本端做出的。
func resolve_remote(call_id: String, _outcome: String) -> bool:
	if not visible or call_id == "" or call_id != _call_id:
		return false
	_call_id = ""
	_stop_countdown()
	visible = false
	return true


func _decide(decision: String) -> void:
	if _call_id == "":
		return
	var cid := _call_id
	_call_id = ""
	_stop_countdown()
	visible = false
	decision_made.emit(cid, decision)


func _start_countdown() -> void:
	_remaining = APPROVAL_WINDOW_SEC
	_update_countdown_label()
	if _tick != null:
		_tick.start(1.0)


func _stop_countdown() -> void:
	if _tick != null:
		_tick.stop()


func _on_tick() -> void:
	if not visible:
		_stop_countdown()
		return
	_remaining -= 1
	_update_countdown_label()
	if _remaining <= 0:
		_stop_countdown()
		# 本地窗口耗尽：强制终态（网关侧同样会超时 cancel；重复的 cancel
		# RPC 会得到 status=unknown，无害）。保证「终态必达」不依赖下行帧。
		_decide("cancel")


func _update_countdown_label() -> void:
	if _countdown == null:
		return
	_countdown.text = "%ds" % maxi(_remaining, 0)
	var col := DshTokens.text_tertiary()
	if _remaining <= 10:
		col = DshTokens.danger()
	elif _remaining <= 20:
		col = DshTokens.warn()
	_countdown.add_theme_color_override("font_color", col)


## §4 minimal motion: quick opacity fade on open/close, layout never animated.
func _fade_open() -> void:
	DshTokens.fade_in(_backdrop, DshTokens.MOTION_BASE)
	DshTokens.fade_in(_card, DshTokens.MOTION_BASE)


func _fade_close() -> void:
	DshTokens.fade_out(_card, DshTokens.MOTION_QUICK, func() -> void: visible = false)
	if _backdrop != null:
		DshTokens.fade_out(_backdrop, DshTokens.MOTION_QUICK, Callable())


## 解析「Allow tool '<name>' with args: <args>?」形态的首部摘要。
## 工具名一行（等宽），目标/命令一行（单行省略号截断）。
## 非 tool 审批（ask_user 自由提问等）解析不出工具名时隐藏摘要块，
## 详情区仍展示完整原文。
func _render_prompt_summary(prompt: String) -> void:
	var tool_name := ""
	var target := ""
	var q0 := prompt.find("'")
	if q0 >= 0:
		var q1 := prompt.find("'", q0 + 1)
		if q1 > q0:
			tool_name = prompt.substr(q0 + 1, q1 - q0 - 1)
			var marker := "with args:"
			var ai := prompt.find(marker, q1)
			if ai >= 0:
				target = prompt.substr(ai + marker.length()).strip_edges()
				target = target.trim_suffix("?").strip_edges()
				var parsed: Variant = JSON.parse_string(target)
				if parsed != null:
					target = JSON.stringify(parsed)
				elif target.contains("\n"):
					target = target.substr(0, target.find("\n"))
	if tool_name != "":
		_tool_label.text = tool_name
		_target_label.text = target
		_summary_box.visible = true
	else:
		_summary_box.visible = false
	_prompt.text = _esc_bb(prompt)


func _rebuild_buttons(options: Array) -> void:
	for c in _btn_row.get_children():
		_btn_row.remove_child(c)
		c.queue_free()
	var mapped: Array = _map_options(options)
	# 主按钮组固定补齐「本会话总是允许」，插在 allow_once 左侧（右端为主）。
	# 仅当本次请求语义上是工具审批时注入；ask_user 自由选项卡不注入。
	var has_allow_all := false
	var is_tool_approval := _tool_label != null and _summary_box != null and _summary_box.visible
	for item in mapped:
		if item is Dictionary and str((item as Dictionary).get("decision", "")) == "allow_all":
			has_allow_all = true
			break
	if not has_allow_all and is_tool_approval:
		var at := mapped.size()
		for i in mapped.size():
			var mi: Dictionary = mapped[i]
			if str(mi.get("decision", "")) == "allow_once":
				at = i
				break
		mapped.insert(at, {"decision": "allow_all", "label": _t("approval.allowAll", "本会话总是允许", "Always allow this session")})

	# 键位门控：快捷键只在对应决策项存在时生效。
	_kbd_allow_once = false
	_kbd_deny = false
	_kbd_allow_all = false
	for item in mapped:
		if not (item is Dictionary):
			continue
		match str((item as Dictionary).get("decision", "")):
			"allow_once":
				_kbd_allow_once = true
			"deny":
				_kbd_deny = true
			"allow_all":
				_kbd_allow_all = true

	for item in mapped:
		if not (item is Dictionary):
			continue
		var d: Dictionary = item
		var decision := str(d.get("decision", "cancel"))
		var btn := Button.new()
		btn.custom_minimum_size = Vector2(110, 34)
		var label := str(d.get("label", decision))
		var hint := str(KEY_HINTS.get(decision, ""))
		btn.text = label if hint == "" else "%s   [%s]" % [label, hint]
		btn.set_meta("decision", decision)
		_style_decision_button(btn, decision)
		btn.pressed.connect(_decide.bind(decision))
		_btn_row.add_child(btn)


func _focus_primary() -> void:
	var kids := _btn_row.get_children()
	for i in range(kids.size() - 1, -1, -1):
		var b := kids[i] as Button
		if b != null:
			b.call_deferred("grab_focus")
			return


func _style_decision_button(btn: Button, decision: String) -> void:
	var bg: Color = DshTokens.bg_layer3()
	var hover: Color = DshTokens.border_l4()
	if decision == "allow_once":
		bg = DshTokens.brand_button()
		hover = DshTokens.text_secondary()
		btn.add_theme_color_override("font_color", DshTokens.bg_base())
		btn.add_theme_color_override("font_hover_color", DshTokens.bg_base())
	elif decision == "deny":
		bg = DshTokens.danger()
		hover = DshTokens.danger()
		btn.add_theme_color_override("font_color", Color(1, 1, 1, 1))
		btn.add_theme_color_override("font_hover_color", Color(1, 1, 1, 1))
	elif decision == "allow_all":
		# 第三主键：次级强调（描边 accent），不与「允许一次」争夺主视觉。
		btn.add_theme_color_override("font_color", DshTokens.accent())
		btn.add_theme_color_override("font_hover_color", DshTokens.accent_hover())
	else:
		btn.add_theme_color_override("font_color", DshTokens.text_primary())
	btn.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(12, 6, 12, 6)))
	btn.add_theme_stylebox_override("hover", DshTokens.box(hover, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(12, 6, 12, 6)))
	btn.add_theme_stylebox_override("pressed", DshTokens.box(hover, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(12, 6, 12, 6)))
	# 键盘焦点环：draw_center=false 只描边，保证全键盘可选可见。
	var focus_sb := DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_MD, DshTokens.accent(), 1, Vector4(12, 6, 12, 6))
	focus_sb.draw_center = false
	btn.add_theme_stylebox_override("focus", focus_sb)


func _map_options(options: Array) -> Array:
	var out: Array = []
	if options.is_empty():
		return [
			{"decision": "cancel", "label": DshI18n.t("approval.cancel")},
			{"decision": "deny", "label": DshI18n.t("approval.reject")},
			{"decision": "allow_all", "label": _t("approval.allowAll", "本会话总是允许", "Always allow this session")},
			{"decision": "allow_once", "label": DshI18n.t("approval.allow")},
		]
	for raw in options:
		var id := ""
		var label := ""
		if raw is Dictionary:
			var d: Dictionary = raw
			id = str(d.get("optionId", d.get("id", d.get("value", d.get("decision", "")))))
			label = str(d.get("name", d.get("label", d.get("title", ""))))
		else:
			id = str(raw)
			label = str(raw)
		var decision := _normalize_decision(id if id != "" else label)
		if label == "":
			label = _label_for(decision)
		out.append({"decision": decision, "label": label})
	if out.is_empty():
		return _map_options([])
	return out


func _normalize_decision(raw: String) -> String:
	var s := raw.strip_edges()
	var lower := s.to_lower().replace(" ", "_")
	if lower == "allow_once" or lower == "allow-once" or lower == "allowonce":
		return "allow_once"
	if lower == "allow" or lower == "y" or lower == "yes" or lower == "a":
		return "allow_once"
	if s.to_lower() == "allow once":
		return "allow_once"
	if lower == "allow_all" or lower == "allow-all" or lower == "allowall" or lower == "always":
		return "allow_all"
	if lower == "deny" or lower == "reject" or lower == "n" or lower == "no" or lower == "d":
		return "deny"
	if lower == "cancel" or lower == "c":
		return "cancel"
	return "cancel" if s == "" else s


func _label_for(decision: String) -> String:
	if decision == "allow_once":
		return DshI18n.t("approval.allow")
	if decision == "allow_all":
		return _t("approval.allowAll", "本会话总是允许", "Always allow this session")
	if decision == "deny":
		return DshI18n.t("approval.reject")
	if decision == "cancel":
		return DshI18n.t("approval.cancel")
	return decision


func _on_backdrop_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			_decide("cancel")


func _input(event: InputEvent) -> void:
	if not visible:
		return
	var k := event as InputEventKey
	if k == null or not k.pressed or k.echo:
		return
	# Steal every key while the card is up so composer Enter/Esc cannot
	# submit or abort around the pending decision.
	match k.keycode:
		KEY_ESCAPE:
			_decide("cancel")
		KEY_1, KEY_KP_1, KEY_Y:
			if _kbd_allow_once:
				_decide("allow_once")
		KEY_2, KEY_KP_2, KEY_N:
			if _kbd_deny:
				_decide("deny")
		KEY_3, KEY_KP_3, KEY_A:
			if _kbd_allow_all:
				_decide("allow_all")
	get_viewport().set_input_as_handled()


func _esc_bb(s: String) -> String:
	return s.replace("[", "[lb]").replace("]", "[rb]")
