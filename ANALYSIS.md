# jsondiff：从节点比较到 patch 应用的等价性审计

本文档审计本仓库（`github.com/wI2L/jsondiff`）从输入归一化到 patch 应用的完整路径，
记录三类真实风险、各选项对操作集（operation set）的影响，以及 round-trip 不变量
`apply(diff(a,b),a)=b` 的成立边界。所有结论均由 `audit_test.go` 与
`testdata/tests/jsonpatch/audit.json` 中的回归材料通过仓库正式公开入口
（`Compare` / `CompareJSON` / `CompareWithoutMarshal` / `Differ.Compare` /
`Patch.Invert`）验证；patch 应用复用仓库既有内部实现 `Patch.apply`
（apply.go:23，与 `differ_test.go` 既有测试共用同一段生产逻辑，未新增平行实现）。

## 1. 完整路径追踪（含文件行号）

1. **输入 normalize**
   - `Compare`（compare.go:11）→ `compare`（compare.go:49）：先经
     `marshalUnmarshal`（compare.go:89）把任意 Go 值 marshal 成 JSON 再 unmarshal
     为 `interface{}`，数字统一归一为 `float64`。
   - `CompareJSON`（compare.go:21）→ `compareJSON`（compare.go:70）：直接对 JSON
     文档 unmarshal；可用 `UnmarshalFunc`（option.go:53）替换，例如 `UseNumber`
     会把数字归一为 `json.Number` 而非 `float64`。
   - `CompareWithoutMarshal`（compare.go:32）：跳过归一化，要求调用方保证值只含
     JSON 原生类型；遇到非法类型时 `deepEqualValue`（equal.go:66）panic，
     由 defer/recover 转成显式错误 `jsondiff: invalid json type: %T`。
2. **节点比较**
   - `Differ.Compare`（differ.go:75）：Factorize 时先 `prepare`（differ.go:156）
     建立「值哈希 → 未变节点指针」的 hashmap；Rationalize 时先压缩 target 字节。
   - `diff`（differ.go:107）：`areComparable`（equal.go:54）按 `jsonTypeSwitch`
     （equal.go:30）判型；不可比较且在根节点 → 生成空路径 `add`（根替换）；
     不可比较且在子节点 → `replace`；可比较但 `deepEqual`（equal.go:58）为假 →
     分派数组/对象/标量比较。
3. **数组算法**
   - 默认逐索引：`compareArrays`（differ.go:276），删除操作复用同一「删除路径」，
     追加操作复用 `/-` 路径。
   - LCS：`compareArraysLCS`（differ.go:331）+ `lcs`（lcs.go:7），用
     `adds-removes` 调整索引。
   - 无序等价：`unorderedDeepEqualSlice`（differ.go:439），以值哈希集合+计数判定。
4. **operation 生成与排序**
   - 对象键在 `compareObjects`（differ.go:231）中经 `sortStrings`（differ.go:553）
     按字典序遍历，因此操作顺序与输入 JSON 的键序、Go map 迭代顺序无关。
   - 操作由 `replace`/`add`/`remove`（differ.go:463/472/504）经
     `Patch.append`/`insert`（operation.go:107/118）追加；Factorize 时 `add`
     会先查 `findRemoved`（differ.go:522）尝试升级为 `move`，再查
     `findUnchanged`（differ.go:511）尝试降级为 `copy`。
   - Rationalize 在 `rationalize`（differ.go:200）中把多个小操作折叠为单个
     `replace`（按 JSON 序列化长度比较代价）。
5. **patch apply**
   - `Patch.apply`（apply.go:23）：克隆源文档后顺序应用；`toDotPath`
     （apply.go:143）把 RFC 6901 指针翻译成 sjson 点路径，数字片段需查源文档
     区分数组索引与对象键；`test` 操作仅检查路径存在；任一操作失败即整体返回
     错误，不向调用方暴露半应用的文档。
   - 逆操作：`Patch.Invert`（patch.go:34）依赖 `test` 操作携带的旧值，
     `copy` 不可逆（`ErrAmbiguousCopyOp`，patch.go:13）。

## 2. 三类真实风险

### 风险一：归一化使「数字表示差异」不可见（或有精度损失）

`Compare`/`CompareJSON` 默认把数字归一为 `float64`：`{"n":1}` 与 `{"n":1.0}`
的 diff 为空 patch；超过 2^53 的整数会发生精度损失。改用
`UnmarshalFunc(UseNumber)` 后，同一对输入产生 1 个 `replace` 操作
（`json.Number("1")` ≠ `json.Number("1.0")`，见 equal.go:30 的
`jsonNumberFloat`/`jsonNumberString` 分派）。
**结论**：round-trip 不变量只在「归一化后的规范形式」上成立，
`apply(diff(a,b),a)` 与 `b` 不保证字节一致；审计测试以
unmarshal→marshal 的规范形式比较（与 differ_test.go:600 的既有约定一致）。

### 风险二：哈希与重复数组元素导致操作集不唯一

- `prepare`（differ.go:156）以 `maphash` 摘要（hash.go:14）为键，**重复等值
  元素互相覆盖**，hashmap 只保留最后一个位置；Factorize 生成的 `move`/`copy`
  的 `from` 因此指向任意一个等值副本——结果正确但不唯一。
- `unorderedDeepEqualSlice`（differ.go:439）用「集合+计数」判定无序等价，
  理论上的哈希碰撞会把不等价数组误判为等价，从而漏发操作。
- Equivalent 下等长无序等价数组的 patch 为空，`apply` 结果在字节上不等于
  target（仅无序等价）。因此**最短 patch 不是唯一正确答案**：同一输入在默认
  （3 个 replace）、LCS（remove+add 共 2 个）、Factorize（1 个 move/copy）下
  产生不同长度的合法操作集，审计测试逐一验证它们都满足 round-trip。

### 风险三：apply 端 test 校验弱化与路径语境依赖

- `test` 操作在 apply.go:23 中只校验路径**存在**（`gjson.Exists`），不比对值：
  把 Invertible patch 应用到旧值不同的文档上仍会成功（审计测试用
  `{"a":999}` 验证了这一弱化语义）。`Patch.Invert` 的正确性依赖 test 携带的
  旧值，但 apply 端并不强制。
- `move` 的源路径不存在时被静默跳过（apply.go:23 中 move 分支的 `break`）。
- `toDotPath`（apply.go:143）需要查阅**源文档**来区分数字是数组索引还是对象
  键；同一份 patch 应用到结构不同的文档上可能被错误翻译。
- apply 在克隆文档上顺序变更、失败即整体报错，不暴露半更新状态；但调用方若
  绕过它逐条自行应用，则会观察到中间态。

## 3. 选项如何改变操作集

| 选项 | 定义 | 对操作集的影响 |
| --- | --- | --- |
| `Equivalent()` | option.go:18 | 等长且无序等价的数组不产生任何操作（patch 可能为空，见风险二） |
| `Factorize()` | option.go:7 | `remove`+`add` 合并为 `move`；新增值命中未变节点时 `add` 变 `copy`（differ.go:472） |
| `Invertible()` | option.go:35 | 每个 `remove`/`replace` 前插入 `test`；禁用 `copy`（copy 不可逆，differ.go:497） |
| `Rationalize()` | option.go:12 | 多个小操作按序列化代价折叠为单个 `replace`（differ.go:200） |
| `LCS()` | option.go:24 | 数组比较从逐索引改为最长公共子序列，操作数通常更少但路径不同 |
| `MarshalFunc`/`UnmarshalFunc` | option.go:43/53 | 改变归一化结果，从而改变整个操作集（例：`UseNumber` 让 `1`/`1.0` 从 0 个操作变为 1 个 `replace`） |

## 4. 失败语义与状态洁净性

- normalize 阶段失败（marshal/unmarshal 出错）：`Compare`/`CompareJSON` 返回
  非 nil error 且 patch 为 nil（compare.go:49/70），不产生任何操作。
- 非法 Go 类型：`CompareWithoutMarshal` 显式返回
  `jsondiff: invalid json type: %T`（compare.go:32），不猜测、不静默成功。
- 非法 JSON 源文档：`Patch.apply(src, true)` 返回 `invalid source document`
  （apply.go:23）。
- `Differ` 比较中途 panic（非法类型）后，`Reset`（differ.go:47）清空 patch
  与 hashmap；审计测试证明：失败调用之后用相同输入重试，结果与从未失败过的
  对照组逐字节一致——成功结果不携带失败调用留下的缓存、计数或临时状态。

## 5. 回归材料索引

- `testdata/tests/jsonpatch/audit.json`：固定输入，经既有 `runCasesFromFile`
  harness 覆盖重复数组元素、数字表示差异、对象键重排、根节点替换。
- `audit_test.go`：
  - `TestAuditCases` / `TestAuditOptionCases`：文件与内联案例（含 move/copy、
    test 操作、LCS、Equivalent 变体）的精确操作序列断言。
  - `TestAuditRoundTrip`：逐案例验证 `apply(diff(a,b),a)=b`，失败时定位首个
    违约操作并打印上下文。
  - `TestAuditInvertibleRoundTrip`：`Invert` 后反向 apply 还原源文档，并验证
    每个 remove/replace 前都有同路径 test。
  - `TestAuditOrderStability`：只改变键序/空白/并发调度（barrier 启动的
    goroutine），patch 序列化结果逐字节一致。
  - `TestAuditOptionsChangeOpSet`：同一输入在不同选项下操作集长度不同但全部
    满足 round-trip（最短 patch 不是唯一正确答案）。
  - `TestAuditEquivalentEmptyPatchRisk` / `TestAuditTestOpWeakValidation`：
    把风险二、风险三固定为可执行断言。
  - `TestAuditRecoverableFailure` / `TestAuditDifferResetAfterPanic`：注入一次
    可恢复失败后重试，结果与对照组一致，无半更新共享状态。
  - `TestAuditExplicitErrors`：无法判定的输入显式返回错误。
  - `TestAuditSeededRoundTrip`：固定 seed 的随机文档 round-trip，失败时打印
    seed 与最小复现输入。
