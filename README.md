# 构建依赖执行引擎

一个基于内容寻址的 Go 增量构建引擎。构建被建模为一组**动作（Action）**，每个动作声明工具、参数、输入文件与产物文件；动作之间的依赖由输入/产物路径关系自动推导，构成有向无环图（DAG）。

## 依赖模型

- **任务/输入/产物关系建图**：产物路径 → 生产者动作的映射推导依赖边，共享前置（如菱形依赖的公共节点）在拓扑序中只执行一次，环会被拒绝。
- **受控动作**：引擎只执行通过 RegisterTool 注册的工具，新工具实现 engine.Tool 接口即可接入同一依赖模型；不做仓库自动发现、不启用仓库自动构建。
- **内容定重建**：动作指纹 = 工具名 + 排序后的参数 + 每个输入文件内容的 SHA-256，不看时间戳。产物还会记录内容哈希，磁盘上被外部改坏或删除的产物会被识别并定点重建。
- **原子发布**：动作在临时暂存目录中产出，全部产物就绪后才 rename 到目标位置；失败不会发布半个产物，也不会污染已记录的状态。
- **重启恢复**：每个动作完成后状态即时落盘（.buildstate.json，原子写），新进程打开同一目录即可继续增量构建。
- **构建途中变化检测**：动作执行前后分别哈希全部输入，若依赖在构建途中被改动，暂存产物直接丢弃、不发布、不记账——该产物不属于任何一致的源码视图，必须重建。
- **可保存的构建视图**：SaveView 把一次 BuildReport 的结果（每个动作实际使用的输入/参数/产物内容哈希，含失败部分）保存到 .views/<name>.json。视图不可变：同名保存被拒绝，View(name) 永远返回当初保存的记录，后来的源码变化不会悄悄替换它；VerifyView 只报告当前工作区与视图的漂移，不修改视图。
- **实验候选**：ForkCandidate 从当前已知状态复制出独立候选（.candidates/<name>/），各自持有独立状态文件，候选之间不会串用缓存（内容哈希一致才可复用，即可用）。AdoptCandidate 把候选记录的源码修改与产物应用回主工作区；候选目录及其视图（含失败依据）原样保留。CompareViews 按动作对比两个视图，指出真正不同的输入与产物。
- **构建影响对照**：CompareViewImpacts 只读取两个已保存 View 中的事实，按原构建目标归因直接输入、共享依赖产物、工具身份/参数变化，并标出仍可复用的动作；失败、取消、跳过或缺少记录的动作单独列为无结论，不会为旧构建重新读取当前文件补写原因。
- **构建计划预览**：NewBuildPlan 从影响对照中选择部分目标，预览闭包、共同依赖、当前可复用动作和可能重复的工作；RefreshPlan 在确认前重新检查输入，返回变化文件以及新变为可复用/必须执行的动作范围，原预览和旧视图均不被修改。
- **构建报告与解释**：BuildReport(ctx, ...) 收集每个动作的状态（ran/reused/failed/skipped/cancelled）与来源哈希，单个目标失败不阻塞无关目标；ctx 取消后不再调度新动作，已完成动作的状态已落盘，重开后续跑。每个动作记录工具身份、参数、依赖来源（DependsOn/InputFrom）；已开始但未成功的动作（失败或输入漂移）还保留开始执行时捕获的输入指纹与输入内容哈希，未开始的动作（取消/跳过）以 Started=false 明确区分，不会事后用当前文件内容补写。Explain(id) 基于落盘状态逐条说明产物可复用的原因，或哪个输入/参数/产物变化真正导致了重建。

## 用法

    e, _ := engine.New(root)
    e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
    e.RegisterTool("upper", engine.ToolFunc(tools.Upper))
    e.RegisterTool("concat", engine.ToolFunc(tools.Concat))

    e.AddAction(engine.Action{ID: "gen", Tool: "copy",
        Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}})
    // ... 更多动作，依赖由 Inputs/Outputs 自动连接
    err := e.Build("merge") // 只重跑输入/参数/产物发生变化的动作

    // 带明细的构建：部分失败继续、可取消、记录来源
    rep := e.BuildReport(ctx, "merge")
    e.SaveView("release-1", rep)        // 保存不可变视图
    drift, _ := e.VerifyView("release-1") // 之后核对工作区是否仍对应该视图

    // 从已知状态试验两组修改并比较
    a, _ := e.ForkCandidate("exp-a")
    b, _ := e.ForkCandidate("exp-b")
    // ... 在各自目录改源码后分别 BuildReport ...
	diff := engine.CompareViews(viewA, viewB)
	impact := engine.CompareViewImpacts(viewA, viewB)
	plan, _ := e.NewBuildPlan(impact, "merge") // 不执行，只预览共享依赖与重复工作
	refresh, _ := e.RefreshPlan(plan)          // 确认前输入又变化时，更新受影响范围
	e.AdoptCandidate("exp-a") // 采用 a；b 的失败依据与产物保留

## 测试

    go test ./...

覆盖：菱形依赖（共享前置只跑一次）、局部修改后的增量重建、未受影响动作不重跑、参数变更只波及下游、产物被外部改坏后的定点恢复、失败后无半个产物、重启恢复、增量结果与全量构建一致、环检测。
