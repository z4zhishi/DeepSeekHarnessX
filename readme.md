# DeepSeekHarnessX (hereinafter referred to as DSHX)

[中文](readme_zh.md) | English

> **DSHX is a high-performance alternative to [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)**
>
> This project completely rebuilds the core architecture with GO + GODOT to maximize throughput and runtime efficiency
>
> Note: Although this project is deeply inspired by [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness), due to significant cross-language differences, it is not fully compatible with the existing ecosystem of the original project
>
> At the same time, I have preserved the high extensibility of the original project and further enhanced it with advanced features that fit actual current usage. Thanks to its high performance baseline, it offers an extremely high ceiling for customization when used properly

> ## Preface
>
>> Since the original project is driven by TypeScript / Node, this offers great flexibility and lowers the development barrier
>>
>> However, the accompanying performance issues — high memory footprint, low execution efficiency of tools, and UI rendering stutter under high TPS — actually keep the model working very slowly
>>
>> When you need to move fast, a large amount of time is spent waiting for various tools to complete, and as more agents or sessions are opened, memory capacity requirements grow higher and higher
>>
>> Therefore, I attempted to rebuild the entire backend with GO while keeping a high degree of customizability, and use Godot as the frontend renderer to achieve optimal graphics performance
>>
>> The result is DSHX, this high-performance edition

## Relationship with DSH



## Version Support

|Mode| Runtime Environment          | Compatibility |
|---|--------------------------|-------|
|GUI| Windows 10 and above         | Supported    |
|GUI| Linux                | Partially supported  |
|TUI| Windows 10 and above         | Supported    |
|TUI| Linux (only Debian 11 tested so far) | Partially supported  |



## Quick Start

The project is not yet complete enough for all features to be fully functional; no prebuilt binaries are provided at this time


## Building from Source

The project is iterating rapidly; not yet available



## License

DSHX is released under the **GPLv2** license

---

## Acknowledgements

### [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)


The backend architecture of DSHX draws heavily on its excellent design patterns. Special thanks to the DeepSeek team and the open-source contributors of the original project

### [Godot Engine](https://godotengine.org) · [Source](https://github.com/godotengine/godot)

The DSHX frontend abandoned the original Web architecture and was rebuilt on the powerful Godot 4 engine, achieving better rendering performance and interaction experience in GUI mode

---
>Disclaimer: This open-source project (DSHX) is **NOT** an official product of DeepSeek or Godot.
>
>It is neither approved by nor affiliated with DeepSeek or Godot. DeepSeek and Godot are trademarks of their respective owners
