extends "res://tests/probe_gui_shell_base.gd"

## GUI 壳探针（Goal §3「GUI 壳」行 / W5 布局冻结值）。
##
## 断言四组：
##   1. 冻结常量：DshColumns/DshTokens 常量 = Goal W5 数值（640/264/280/420/56/…）
##   2. compute_columns：多档宽度下遵守 CENTER_MIN，放不下时侧栏降级 rail 56，
##      details 让位
##   3. 三栏结构：SidebarSlot + Center + Details 同挂 %Frame；Sidebar 装在
##      SidebarSlot 槽内；Header/ChatTab/ComposerSeat 都在 Center 下
##   4. Hero=DSHX + 空态切换：%Hero 是 engine 挂载的 HeroView（带 reconciler
##      根 meta，磁盘 tscn 子树被弃）且文案含 DSHX；空会话时 hero 可见 /
##      ChatList 隐藏，来实时事件后两者翻转
##
## RESULT 标签：GUI_SHELL_RESULT passed=N failed=M

const DshReconRef := preload("res://engine/reconciler.gd")


func result_tag() -> String:
	return "GUI_SHELL_RESULT"


func _run() -> void:
	_verify_constants()
	_verify_columns_solver()
	var loaded := await boot_app([{"id": "s1", "title": "Shell 会话", "cwd": "C:/repo"}], {"s1": []}, [])
	if not loaded:
		_assert(false, "app.tscn + mock 网关装载")
		return
	await _verify_structure()
	await _verify_columns_applied()
	await _verify_hero_and_empty_state()


# 1 ---------------------------------------------------------------------------

func _verify_constants() -> void:
	print("== 冻结常量（Goal W5） ==")
	_assert(is_equal_approx(DshColumns.CENTER_MIN, 640.0), "DshColumns.CENTER_MIN == 640")
	_assert(is_equal_approx(DshColumns.SIDEBAR_MIN, 264.0), "DshColumns.SIDEBAR_MIN == 264")
	_assert(is_equal_approx(DshColumns.SIDEBAR_DEFAULT, 280.0), "DshColumns.SIDEBAR_DEFAULT == 280")
	_assert(is_equal_approx(DshColumns.SIDEBAR_MAX, 420.0), "DshColumns.SIDEBAR_MAX == 420")
	_assert(is_equal_approx(DshColumns.SIDEBAR_COLLAPSED, 56.0), "DshColumns.SIDEBAR_COLLAPSED == 56")
	_assert(is_equal_approx(DshColumns.SIDEBAR_AUTO_COLLAPSE, 1024.0),
			"DshColumns.SIDEBAR_AUTO_COLLAPSE == 1024")
	_assert(is_equal_approx(DshColumns.DETAILS_MIN, 300.0), "DshColumns.DETAILS_MIN == 300")
	_assert(is_equal_approx(DshColumns.DETAILS_DEFAULT, 360.0), "DshColumns.DETAILS_DEFAULT == 360")
	_assert(is_equal_approx(DshColumns.DETAILS_MAX, 520.0), "DshColumns.DETAILS_MAX == 520")
	_assert(is_equal_approx(DshTokens.SIDEBAR_RAIL, 56.0), "DshTokens.SIDEBAR_RAIL == 56")
	_assert(is_equal_approx(DshTokens.CHAT_CONTENT_WIDTH, 748.0), "DshTokens.CHAT_CONTENT_WIDTH == 748")
	_assert(is_equal_approx(DshTokens.COMPOSER_MAX, 780.0), "DshTokens.COMPOSER_MAX == 780")
	_assert(is_equal_approx(DshTokens.SIDEBAR_DEFAULT, DshColumns.SIDEBAR_DEFAULT)
			and is_equal_approx(DshTokens.DETAILS_DEFAULT, DshColumns.DETAILS_DEFAULT),
			"DshTokens 与 DshColumns 的侧栏/details 冻结值一致")


# 2 ---------------------------------------------------------------------------

func _verify_columns_solver() -> void:
	print("== compute_columns 求解行为 ==")
	var wide: Dictionary = DshColumns.compute_columns(1440.0, DshColumns.SIDEBAR_DEFAULT, 0.0)
	_assert(is_equal_approx(float(wide["sidebar"]), 280.0), "1440px pref280 -> sidebar 280")
	_assert(is_equal_approx(float(wide["center"]), 1160.0), "1440px center 1160（>= CENTER_MIN）")
	_assert(is_equal_approx(float(wide["details"]), 0.0), "1440px details_pref 0 -> details 关")
	var tight: Dictionary = DshColumns.compute_columns(720.0, DshColumns.SIDEBAR_DEFAULT, 0.0)
	_assert(is_equal_approx(float(tight["sidebar"]), 56.0),
			"720px 放不下侧栏+中心 -> 侧栏降级 rail 56")
	_assert(is_equal_approx(float(tight["center"]), 664.0), "720px center 664 >= CENTER_MIN")
	var with_details: Dictionary = DshColumns.compute_columns(1440.0, 280.0, 360.0)
	_assert(is_equal_approx(float(with_details["details"]), 360.0), "1440px details_pref 360 -> details 360")
	_assert(float(with_details["center"]) >= DshColumns.CENTER_MIN, "details 打开后 center 仍 >= CENTER_MIN")
	var too_narrow: Dictionary = DshColumns.compute_columns(900.0, 280.0, 360.0)
	_assert(is_equal_approx(float(too_narrow["details"]), 0.0), "900px 放不下 details -> details 让位为 0")


# 3 ---------------------------------------------------------------------------

func _verify_structure() -> void:
	print("== 三栏结构 ==")
	var frame := app.get_node_or_null("%Frame") as HBoxContainer
	var slot := app.get_node_or_null("%SidebarSlot") as Control
	var center := app.get_node_or_null("%Center") as PanelContainer
	var details := app.get_node_or_null("%Details") as Control
	if frame == null or slot == null or center == null or details == null:
		_assert(false, "Frame/SidebarSlot/Center/Details 四个成员齐备")
		return
	_assert(slot.get_parent() == frame and center.get_parent() == frame
			and details.get_parent() == frame,
			"SidebarSlot + Center + Details 同挂 %Frame（三栏同级）")
	var sidebar := app.get_node_or_null("%Sidebar") as Control
	_assert(sidebar != null and sidebar.get_parent() == slot,
			"%Sidebar 实例装在 SidebarSlot 槽内（槽 owns rail 宽度）")
	var header := app.get_node_or_null("%Header") as Control
	var chat_tab := app.get_node_or_null("%ChatTab") as Control
	var composer := app.get_node_or_null("%Composer") as Control
	_assert(header != null and center.is_ancestor_of(header), "%Header 在 Center 主栏内")
	_assert(chat_tab != null and center.is_ancestor_of(chat_tab), "%ChatTab 在 Center 主栏内")
	_assert(composer != null and center.is_ancestor_of(composer),
			"%Composer（ComposerBar）在 Center 主栏输入席位上")
	var chat := app.get_node_or_null("%ChatList") as Control
	var hero := app.get_node_or_null("%Hero") as Control
	_assert(chat_tab != null and chat != null and chat_tab.is_ancestor_of(chat)
			and hero != null and chat_tab.is_ancestor_of(hero)
			and chat.get_parent() == hero.get_parent(),
			"%ChatList 与 %Hero 为同层兄弟（空态由 HeroView 独立承担）")


# 4 ---------------------------------------------------------------------------

func _verify_columns_applied() -> void:
	print("== 应用树内实测列宽 ==")
	DisplayServer.window_set_size(Vector2i(1440, 900))
	app.get_viewport().size = Vector2i(1440, 900)
	await _frames(8)
	app._apply_columns()
	await _frames(8)
	var vp := app.size.x
	var expect_narrow := vp <= DshTokens.SIDEBAR_AUTO_COLLAPSE
	var expect_sidebar := DshColumns.SIDEBAR_COLLAPSED if expect_narrow else DshColumns.SIDEBAR_DEFAULT
	var applied: Dictionary = app._applied_cols
	_assert(app._narrow == expect_narrow,
			"窄断点（<=1024）与实测视口 %.0fpx 一致" % vp)
	_assert(is_equal_approx(float(applied.get("sidebar", -1.0)), expect_sidebar),
			"实测 sidebar=%s（冻结 pref 或 rail 降级）" % str(expect_sidebar))
	_assert(float(applied.get("center", 0.0)) >= DshColumns.CENTER_MIN,
			"实测 center >= CENTER_MIN")
	_assert(bool(applied.get("details_open", true)) == false and not app._details.visible,
			"细节窗默认关闭（details_open=false 且 %Details.visible=false）")
	var slot := app.get_node_or_null("%SidebarSlot") as Control
	_assert(slot != null and is_equal_approx(slot.custom_minimum_size.x, expect_sidebar),
			"SidebarSlot custom_minimum_size.x == 实测侧栏宽（槽 owns rail）")
	# 点开工具详情 -> details 走冻结值；放不下时让位（solver 语义走真实 app 路径）。
	app._on_tool_selected("probe-call", "fs.read", "{}", "ok")
	await _frames(10)
	app._apply_columns()
	await _frames(4)
	var opened: Dictionary = app._applied_cols
	var fits_details := vp >= DshColumns.SIDEBAR_DEFAULT + DshColumns.DETAILS_DEFAULT + DshColumns.CENTER_MIN
	if fits_details:
		_assert(bool(opened.get("details_open", false)) == true and app._details.visible,
				"tool_selected 打开详情窗（details_open=true）")
		_assert(is_equal_approx(float(opened.get("details", 0.0)), 360.0),
				"详情窗宽度走冻结值 DETAILS_DEFAULT 360")
		_assert(float(opened.get("center", 0.0)) >= DshColumns.CENTER_MIN,
				"详情打开后 center >= CENTER_MIN")
	else:
		_assert(is_equal_approx(float(opened.get("details", 1.0)), 0.0),
				"视口放不下 details -> 实测让位为 0（CENTER_MIN 优先）")
	app._close_details()
	await _frames(4)
	_assert(bool(app._applied_cols.get("details_open", true)) == false and not app._details.visible,
			"close_requested 路径回落 details 关闭")
	# 窄断点翻转（AUTO_COLLAPSE=1024）：同一冻结语义在响应式下仍成立
	DisplayServer.window_set_size(Vector2i(800, 600))
	app.get_viewport().size = Vector2i(800, 600)
	await _frames(8)
	app._apply_columns()
	await _frames(8)
	var vp2 := app.size.x
	# headless 下 DisplayServer 缩窗不一定贴到请求值；断言按"实测宽度"对齐
	# 冻结语义（<1024 应收敛 rail、>1024 应保 DEFAULT），不假装真的到过 800。
	_assert(app._narrow == (vp2 <= DshTokens.SIDEBAR_AUTO_COLLAPSE),
			"改窗后 _narrow 与实测视口 %.0fpx 一致（AUTO_COLLAPSE=1024 语义一致）" % vp2)
	_assert(is_equal_approx(float(app._applied_cols.get("sidebar", -1.0)),
			DshColumns.SIDEBAR_COLLAPSED if app._narrow else DshColumns.SIDEBAR_DEFAULT),
			"实测侧栏宽随断点取 rail 56 / DEFAULT 280")
	_assert(float(app._applied_cols.get("center", 0.0)) >= DshColumns.CENTER_MIN or app._narrow == false,
			"窄断点下 center 仍守住 CENTER_MIN")


func _verify_hero_and_empty_state() -> void:
	print("== Hero=DSHX 与空态切换 ==")
	var hero := app.get_node_or_null("%Hero")
	var chat := app.get_node_or_null("%ChatList")
	if hero == null or chat == null:
		_assert(false, "%Hero / %ChatList 可解析")
		return
	var hero_script: Variant = hero.get_script()
	var hero_script_path := str((hero_script as Script).resource_path) if hero_script is Script else ""
	_assert(hero_script_path == "res://scripts/ui/hero.gd", "%Hero 挂 HeroView 脚本（scripts/ui/hero.gd）")
	var hero_root: Control = null
	if hero.has_meta(DshReconRef.META_ROOT):
		hero_root = hero.get_meta(DshReconRef.META_ROOT) as Control
	_assert(hero_root != null and hero.is_ancestor_of(hero_root),
			"Hero 树由 reconciler 引擎挂载（META_ROOT 落在 HeroView 内）")
	# engine 挂载产物里找标题 Label（hero_doc.gd 的 chat.heroTitle -> DSHX）
	var found_title := ""
	var stack: Array = [hero]
	while not stack.is_empty():
		var cursor: Node = stack.pop_back()
		for child in cursor.get_children():
			if child is Label and str((child as Label).text).find("DSHX") >= 0:
				found_title = str((child as Label).text)
			if child is Node:
				stack.append(child)
	_assert(found_title != "", "Hero 引擎树含 DSHX 标题（HeroView 引擎挂载渲染）")
	_assert(app._hero.visible and not app._chat.visible,
			"空会话首发：Hero 可见、ChatList 隐藏（chat hidden/empty 交换正确）")
	# 注入一条实时会话事件：空态翻转（同一 _show_empty 出口）
	client.call("deliver_session_event", {
		"type": "user/message",
		"seq": 1,
		"data": {"turn": 1, "message": {"id": "m1", "content": [{"type": "text", "text": "壳层探针消息"}]}},
	})
	await _frames(6)
	_assert(app._hero.visible == false and app._chat.visible,
			"来事件后：Hero 隐藏、ChatList 显示（empty state swap）")
	_assert(chat_kinds() == ["user"], "ChatList 折叠出 user 行（kind=user）")