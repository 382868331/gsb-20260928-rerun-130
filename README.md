# 构建依赖执行引擎

一个基于内容寻址的 Go 增量构建引擎。构建被建模为一组**动作（Action）**，每个动作声明工具、参数、输入文件与产物文件；动作之间的依赖由输入/产物路径关系自动推导，构成有向无环图（DAG）。

## 依赖模型

- **任务/输入/产物关系建图**：产物路径 → 生产者动作的映射推导依赖边，共享前置（如菱形依赖的公共节点）在拓扑序中只执行一次，环会被拒绝。
- **受控动作**：引擎只执行通过 RegisterTool 注册的工具，新工具实现 engine.Tool 接口即可接入同一依赖模型；不做仓库自动发现、不启用仓库自动构建。
- **内容定重建**：动作指纹 = 工具名 + 排序后的参数 + 每个输入文件内容的 SHA-256，不看时间戳。产物还会记录内容哈希，磁盘上被外部改坏或删除的产物会被识别并定点重建。
- **原子发布**：动作在临时暂存目录中产出，全部产物就绪后才 rename 到目标位置；失败不会发布半个产物，也不会污染已记录的状态。
- **重启恢复**：每个动作完成后状态即时落盘（.buildstate.json，原子写），新进程打开同一目录即可继续增量构建。

## 用法

    e, _ := engine.New(root)
    e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
    e.RegisterTool("upper", engine.ToolFunc(tools.Upper))
    e.RegisterTool("concat", engine.ToolFunc(tools.Concat))

    e.AddAction(engine.Action{ID: "gen", Tool: "copy",
        Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}})
    // ... 更多动作，依赖由 Inputs/Outputs 自动连接
    err := e.Build("merge") // 只重跑输入/参数/产物发生变化的动作

## 测试

    go test ./...

覆盖：菱形依赖（共享前置只跑一次）、局部修改后的增量重建、未受影响动作不重跑、参数变更只波及下游、产物被外部改坏后的定点恢复、失败后无半个产物、重启恢复、增量结果与全量构建一致、环检测。

