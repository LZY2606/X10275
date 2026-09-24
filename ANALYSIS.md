# jsondiff：从节点比较到 patch 应用的等价性审计

本文档追踪 `diff(a, b)` 从输入 normalize 到 patch apply 的完整路径，记录三类真实风险，
并说明各选项如何改变操作集。配套的回归材料在 `audit_test.go`，所有用例只通过仓库的
公开入口（`Compare` / `CompareJSON` / `CompareWithoutMarshal` / `Differ.Compare`）进入，
不伪造内部最终对象。

## 1. 完整路径追踪追踪

### 1.1 入口与 normalize（输入规范化）

- `Compare`（compare.go:11）、`CompareJSON`（compare.go:21）、
  `CompareWithoutMarshal`（compare.go:32）是全部公开入口。
- `compare`（compare.go:49）对任意 Go 值先走 `marshalUnmarshal`（compare.go:89）：
  先 marshal 成 JSON 字节，再 unmarshal 回 `interface{}`，把输入规范化为
  `json.Unmarshal` 产生的类型闭包（`float64` / `string` / `bool` /
  `[]interface{}` / `map[string]interface{}` / `nil`）。
- `compareJSON`（compare.go:70）跳过 marshal，直接 unmarshal 原始字节；
  目标文档的原始字节存入 `d.targetBytes`，供 Rationalize 计算操作成本。
- Rationalize 启用且输入未压缩时，先经 `compact` / `compactInPlace`
  （json.go:126、json.go:133）去除空白，见 `Differ.Compare`（differ.go:75）。

### 1.2 节点比较

- `Differ.Compare`（differ.go:75）在 Factorize 启用时先 `prepare`（differ.go:156）
  建立“值哈希 → 未变更路径”的 hashmap，然后进入 `diff`（differ.go:107）。
- `diff` 先用 `areComparable`（equal.go:54）判断类型是否同类：根节点不可比较时
  生成一条空路径的 `add`（整文档替换，differ.go:113-117）；非根节点走
  `replace`（differ.go:463）。同类但 `deepEqual`（equal.go:58）为真则不产生操作。
- 标量差异在 `diff` 的 `default` 分支生成 `replace`；Invertible 启用时先插一条
  `test`（differ.go:463-470）。

### 1.3 数组算法

- 默认算法 `compareArrays`（differ.go:276）：索引对齐逐一比较；源更长时尾部用
  固定 removal path 批量 `remove`，目标更长时用 `/-` 追加路径批量 `add`。
- LCS 算法 `compareArraysLCS`（differ.go:331）：`lcs`（lcs.go:7）用动态规划求
  最长公共子序列的下标对，未匹配区段按“双箭头”推进生成 remove/add/replace。
- `Equivalent` 启用且两数组等长时，先经 `unorderedDeepEqualSlice`
  （differ.go:439）做无序判等，相等则整个数组不产生操作（differ.go:287、
  differ.go:333）。

### 1.4 operation 排序

- 对象键在 `compareObjects`（differ.go:231）中经 `sortStrings`（differ.go:553）
  按字典序排序后逐一比较，因此操作顺序与 Go map 迭代顺序、JSON 文本键序无关。
- 数组操作按索引单调推进；LCS+Factorize 时 `move` 通过 `Patch.insert`
  （operation.go:118）插到 `snapshotPatchLen` 处，保持 apply 时索引语义正确。
- `rationalize`（differ.go:200）按 `jsonLength`（operation.go:73）估算成本，
  若一条整替换比若干小操作更短则折叠；成本相同不折叠。

### 1.5 patch apply

- `Patch.apply`（apply.go:23）是仓库内部（测试专用）应用器：先 `bytes.Clone`
  （apply.go:27）复制源文档，再逐条把 JSON Pointer 经 `toDotPath`（apply.go:143）
  转成 gjson/sjson 的 dot-path 执行。失败时返回错误，调用方持有的源文档字节
  不会被半更新。
- 注意：`apply` 的 `test` 分支（apply.go:71 起）只校验路径存在性，不校验值相等；
  它只是内部测试辅助，不是 RFC6902 的完整实现（apply.go:14-21 的注释明确说明
  永不导出）。

## 2. 三类真实风险

### R1 数字表示差异被 normalize 吞掉（compare.go:70、hash.go:38）

默认 unmarshal 把 JSON 数字一律解析为 `float64`：`1` 与 `1.0` 判等、
`9007199254740992` 与 `9007199254740993`（> 2^53）判等，diff 返回空 patch，
尽管字节层面不同。`hasher.hash`（hash.go:38）同样按 float64 位模式哈希，
Factorize/Equivalent 的判等一并受影响。缓解：`UnmarshalFunc` 注入
`UseNumber` 的解码器后，数字按 `json.Number` 字面字符串比较（equal.go:106），
操作集从空变为一条 `replace`。回归：`TestAuditRiskJudgments/number_*`。

### R2 Equivalent 对重复元素数组的多重集误判（differ.go:439）

`unorderedDeepEqualSlice` 用 `map[uint64]struct{}` 记录元素哈希集合，只校验
“目标每个元素的哈希都在集合中”以及总数相等，不校验每个值出现的次数。
因此 `[1,1,2]` 与 `[1,2,2]` 在 `Equivalent()` 下被判等、产生空 patch，
round-trip 不变量 `apply(diff(a,b),a)=b` 在该选项下不成立。这是本审计确认
并固化的真实风险：使用 `Equivalent` 即接受“无序集合语义”，重复元素计数
不参与判等。回归：`TestAuditRiskJudgments/equivalent_duplicate_multiset`。

### R3 patch 非唯一，最短 patch 不是唯一正确答案（lcs.go:7、differ.go:200）

- LCS 在含重复元素时配对不唯一：回溯阶段（lcs.go:24-33）在 `t[i-1][j] ==
  t[i][j-1]` 时固定走 `j--`，换一组同样合法的配对会产生不同但同样正确的
  patch。同一输入在默认算法（`[replace /1]`）与 LCS（`[remove /0, add /1]`）
  下操作集本就不同。
- `rationalize`（differ.go:200）以 JSON 字节成本折叠操作，成本估计依赖
  `valueLen`，不改变语义但改变操作集形状。
- 因此本审计不以“最短 patch”或某一固定操作序列作为唯一正确标准；正确性
  以两条为准：(a) round-trip 不变量 `apply(diff(a,b),a) ≡ b`（规范化后逐字节
  相等）；(b) 同一入口、同一选项下操作集顺序稳定（见第 4 节）。

## 3. 选项如何改变操作集

- **Equivalent（等值）**：等长数组无序判等成功时整组不产生操作
  （differ.go:287、differ.go:333），并接受 R2 的多重集风险。
- **自定义 marshal（MarshalFunc/UnmarshalFunc，option.go:62-80）**：
  替换 normalize 阶段的编解码器（compare.go:49-56、compare.go:89）。
  注入 `UseNumber` 解码器会把数字比较从 float64 语义切换为字面语义（R1），
  同一对输入的操作集从空变为 `replace`。
- **Factorize**：`prepare`（differ.go:156）建立未变更值索引；`add`
  （differ.go:472）时若值匹配已 `remove` 的节点则改写为 `move`
  （`findRemoved`，differ.go:522），若匹配未变更节点则改为 `copy`
  （`findUnchanged`，differ.go:511）；`hasProperPathPrefix`（differ.go:532）
  阻止把节点移入自己的子节点。
- **Invertible**：每条 `remove`/`replace` 前插入 `test`（differ.go:463-470、
  differ.go:504-509），并禁用 `copy`（differ.go:513 的 `!d.opts.invertible`
  分支退化为 `add`），保证 `Patch.Invert`（patch.go:34）能逐对消费
  test+操作生成逆 patch。

## 4. 正确性判定与状态清洁契约

- **不以最短 patch 为唯一正确答案**（见 R3）。验收不变量：
  `apply(diff(a,b),a)` 规范化后等于 `b`；同一输入在改变 JSON 文本键序、
  Go map 插入顺序、并发调度时，操作集逐字节一致。
- **无法判定的输入显式报错，不猜成成功**：`CompareWithoutMarshal` 对非 JSON
  类型闭包的输入返回 `jsondiff: invalid json type: <T>`（compare.go:32-47，
  panic 点在 equal.go:69-73）；非法 JSON 字节由 unmarshal 错误直接返回。
- **可恢复失败与状态清洁**：normalize 阶段的失败（marshal/unmarshal 错误）
  发生在任何状态变更之前，重试即干净；比较中途的失败（invalidJSONTypeError
  panic）可能在 `d.patch` 中留下已生成的操作，恢复路径是 `Differ.Reset`
  （differ.go:47），重置后重试的结果与全新 `Differ` 逐字节一致，不携带失败
  调用留下的缓存、计数或临时状态。`Patch.apply` 失败时调用方源文档不被
  半更新（apply.go:27 的 `bytes.Clone`），相同输入重试得到相同错误。

## 5. 回归材料索引（audit_test.go）

- `TestAuditRoundTripFixedInputs`：固定输入覆盖重复数组元素、数字表示差异、
  对象键重排、根节点替换、move/copy、test operation，逐条验证操作序列与
  round-trip 不变量。
- `TestAuditRiskJudgments`：固化 R1/R2 风险判断（含 R2 下 round-trip 必然
  失败的反向证明）。
- `TestAuditDeterministicOperationOrder` / `TestAuditConcurrentCompareStable`：
  只改变与结果无关的枚举/调度条件，证明操作顺序稳定（固定 seed，失败时打印
  最小复现）。
- `TestAuditRecoverableFailure` / `TestAuditApplyFailureAtomicity` /
  `TestAuditUnsupportedInputExplicitError`：失败注入、恢复重试、状态清洁与
  显式不支持契约。
