// Package core 是 DSHX 插件层的候选抽象：生命周期、能力发现、资源归属，
// 以及事件/任务/权限等进程无关运行时原语。具体工具、命令、事件、UI 提供方
// 一律视为插件，必须通过本包的公开契约注册。
//
// 【非生产宿主】本包当前不接入任何产品入口：backend/cmd 没有任何路径构造
// core.NewRegistry；生产插件宿主是 pkg/plugin.Registry（见
// docs/reconstruction-goal.md §1/§7 与 §W9-b 验收）。本包及其子包（含
// easyapi）仅供隔离验证与未来"单一宿主"接入评估使用；本包测试全绿不等于
// 生产接入完成。
//
// 与生产侧的唯一现有触点是 pkg/plugin.CoreBridge：它把 pkg/plugin 生产注册表
// 适配到本包 API 之上，仅做视图转发；生产插件装载不经过 core.Registry。
package core