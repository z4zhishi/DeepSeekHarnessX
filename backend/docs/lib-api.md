# Lib Registry（lib-api）

## 定位

Lib 是面向开发者的**可声明依赖层**（plan §Lib registry）：第三方插件通过
manifest 声明 `libs`，Core 在加载时解析版本约束并注入对应 facade。

## Manifest 声明

```json
{
  "name": "my-plugin",
  "abiVersion": 1,
  "libs": [
    { "id": "dshx-easy-api", "version": "^1.0" }
  ]
}
```

## 解析规则

| 约束 | 语义 |
|---|---|
| `^1.0` | 兼容窗口：`1.x >= 1.0` |
| 缺失 lib | 插件加载失败，`lastErr` 记录（面板可观察），不静默降级 |
| 未注册 lib id | 同上 |
| 循环依赖 | 拒绝加载并报告环 |
| reload | 重载前检查该 lib 被哪些插件依赖；依赖方收到 isolated 拒绝或提示 |

## 内置库清单

| lib id | 载体 | 状态 |
|---|---|---|
| `dshx-easy-api` | `backend/core/easyapi`（独立可选包） | 1.0 |

其余候选（markdown / json / validation / ui helpers）在 plan 中列为未来可
拆分项，当前不预置。

## Lib 能力的资源预算

lib 本身不注册工具/命令；它只注入 API facade。资源预算（事件频率、任务
配额）跟随**使用方插件**的 owner 走，使卸载/统计保持在插件粒度。