# DeepSeekHarnessX（以下简称DSHX）

中文 | [English](readme.md)

> **DSHX是[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)的高性能替代方案**
> 
> 本项目使用GO+GODOT彻底重构了核心架构以最大化吞吐量和运行效率
> 
>注意: 虽然本项目深受[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)启发,但它在生态兼容性方面由于跨语言差异较大,它不完全兼容原项目的现有生态
>
>同时我保留了原项目的高度可扩展性,并进一步增强添加了一些契合当前实际使用的高级特性,得益于较高的性能基准,在合理使用的情况下其拥有极高的自定义上限

> ## 前言
>
>>由于原项目使用TypeScript / Node驱动,这尽管有很大的自由度并降低了开发门槛
>>
>>但随之而来的高内存占用,低执行效率工具,面板在高TPS情况下的渲染卡顿等性能问题实际上让模型一直工作的非常慢
>>
>>当你需要快速推进时有大量的时间是在等待各种工具完成,同时agent或会话开启较多时对内存容量的要求越来越高
>>
>>因此我尝试在尽可能高度可自定义的情况下使用GO完全重构了整个后端,并使用Godot为前端渲染器以获得最佳图形性能
>>
>>最终得到了DSHX这一高性能版本

## 与 DSH 的关系



## 版本支持

|模式| 运行场景                 | 实际兼容性 |
|---|----------------------|-------|
|GUI| Windows10及以上         | 支持    |
|GUI| Linux                | 部分支持  |
|TUI| Windows10及以上         | 支持    |
|TUI| Linux(暂仅测试了Debian11) | 部分支持  |



## 快速开始

当前项目暂未完善到所有功能完全有效,暂不提供构建成品


## 从源码构建

项目正在快速迭代,暂不提供



## 许可

DSHX 以 **GPLv2** 许可发布

---

## 致谢 (Acknowledgements)

### [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)


DSHX 的后端架构深度参考了其优秀的设计模式,特此向 DeepSeek 团队及原项目的开源贡献者致敬

### [Godot Engine](https://godotengine.org) · [源码](https://github.com/godotengine/godot)

DSHX 的前端摒弃了原有的 Web 架构,基于强大的 Godot 4 引擎进行了重构,从而在 GUI 模式下实现了更优的渲染性能与交互体验

---
>声明: 本开源项目(DSHX)  **不是**  DeepSeek 或 Godot 官方的产品.
> 
>未经 DeepSeek 或 Godot 批准或与之关联.DeepSeek,Godot 分别为其各自所有者的商标
