# jsondiff 等价性审计：从节点比较到 patch 应用

本文件审计 `apply(diff(a, b), a) == b` 这一 round-trip 不变量在本仓库中的完整路径，
记录三类真实风险，并说明各选项如何改变操作集。所有结论均由 `audit_test.go` 中的
回归测试以最小输入复现并锁定。

## 1. 完整路径（真实入口 → 内部阶段）

公开入口（compare.go）：

- `Compare` compare.go:11 → `compare` compare.go:49 → `marshalUnmarshal` compare.go:89
  （输入 normalize：先 marshal 再 unmarshal，把任意 Go 值归一为
  `json.Unmarshal` 产出的类型集合：nil/bool/string/float64/[]interface{}/map[string]interface{}）
- `CompareJSON` compare.go:21 → `compareJSON` compare.go:70（仅 unmarshal，不重新 marshal）
- `CompareWithoutMarshal` compare.go:32（跳过 normalize，非法类型经 recover 转为
  错误返回 `jsondiff: invalid json type: %T`，属于现有错误分层）

内部阶段（differ.go）：

1. **normalize**：`marshalUnmarshal` compare.go:89；数字默认归一为 `float64`，
   词法表示（`1` vs `1.0`）在此阶段被抹平。
2. **节点比较**：`Differ.Compare` differ.go:75 → `diff` differ.go:107 →
   `areComparable` equal.go:54（类型不可比 → 替换/根节点 add）→
   `deepEqual` equal.go:58（相等 → 无操作）。
3. **数组算法**：`compareArrays` differ.go:276（默认，按下标对齐）或
   `compareArraysLCS` differ.go:331（`LCS()` 选项，lcs.go:7）；
   `Equivalent()` 时先走 `unorderedDeepEqualSlice` differ.go:439（基于
   `hasher.digest` hash.go:14 的集合比较）。
4. **operation 排序与生成**：对象键经 `sortStrings` differ.go:553 排序后遍历
   （compareObjects differ.go:231），保证输出顺序稳定；`Factorize()` 时
   `prepare` differ.go:156 预建哈希表，`add` differ.go:472 将 remove+add
   折叠为 move、将重复值折叠为 copy；`Rationalize()` 时 `rationalize`
   differ.go:200 用 `Operation.jsonLength` operation.go:73 估算成本，
   必要时把多个小操作合并为一个 replace；`Invertible()` 在 remove/replace
   前插入 test（differ.go:463、504）。
5. **patch apply**：`Patch.apply` apply.go:23（内部测试入口，未导出，见
   apply.go:16-20 的说明）→ `toDotPath` apply.go:143（JSON Pointer →
   gjson/sjson dot-path，数字键需查源文档区分对象键与数组下标）→
   `add`/`replace` apply.go:90/100（根路径映射为 `@this`）。

## 2. 三类真实风险

### 风险 A：`Equivalent()` 对重复元素数组可能产生不满足 round-trip 的空 patch

`unorderedDeepEqualSlice`（differ.go:439）把元素 digest 放进 map 计数，
`[1,1,2]` 与 `[1,2,2]` 的 digest 集合相同（`{h(1),h(2)}`），计数净额都为 0，
被判为"等价"而返回空 patch —— 但 `apply([], [1,1,2]) = [1,1,2] ≠ [1,2,2]`。
即多重集语义被误判为集合语义。回归：`TestAudit_RoundTrip/duplicate_array_elements`。

### 风险 B：`Equivalent()`/`Factorize()` 会原地修改调用方传入的切片

`hasher.sortArray`（hash.go:61）用 `slices.SortStableFunc` **就地**排序数组，
`unorderedDeepEqualSlice`（differ.go:439，sort=true）与 `prepare` 的 digest
（differ.go:168，sort=false 但嵌套数组仍会被排序）都会触发。比较结束后
调用方的 target 切片元素顺序已被改变（实验中 `[2,1,1]` 保持乱序）。
这是越过 API 边界的副作用；round-trip 语义不受影响，但共享该切片的
其他调用方会观察到突变。回归：`TestAudit_EquivalentSideEffect`。

### 风险 C：`apply` 的 test 操作只验证存在性，不验证值

`Patch.apply`（apply.go:67-80）对 `OperationTest` 仅检查
`gjson.GetBytes(tgt, dp).Exists()`，从不与 `op.Value` 比较。因此
`Invertible()` 生成的 test 操作在 apply 阶段形同虚设：一个值错误的 test
（如对 `{"n":1}` 断言 999）也会通过。test 的真实消费者是
`Patch.Invert`（patch.go:34，用 test 值重建旧值）。任何依赖"apply 会
拒绝值不匹配的 test"的调用方都会得到错误的安全感。
回归：`TestAudit_TestOpRisk`。

## 3. 选项如何改变操作集

| 选项 | 入口 | 对操作集的影响 |
| --- | --- | --- |
| `Factorize()` option.go:7 | `prepare` differ.go:156 + `add` differ.go:472 | remove+add 同值折叠为 `move`；目标中重复出现的值折叠为 `copy`（来源为哈希表中任一相等节点，见 §4）；`Invertible` 下 copy 被禁用回退为 add（differ.go:497） |
| `Rationalize()` option.go:12 | `rationalize` differ.go:200 | 当一组子操作的 JSON 字节成本高于单个 replace 时，整组替换为一个 replace（含 `Invertible` 时的 test）；**不保证最短**，只做局部成本比较 |
| `Equivalent()` option.go:18 | `unorderedDeepEqualSlice` differ.go:439 | 等长数组若"无序等价"则不产生任何操作（风险 A：重复元素时该判断过宽） |
| `LCS()` option.go:24 | `compareArraysLCS` differ.go:331 | 数组改按最长公共子序列对齐，产生 move/add/remove 而非逐下标 replace |
| `Invertible()` option.go:35 | `replace`/`remove` differ.go:463/504 | 每个 remove/replace 前插入 `test`；copy 退化为 add；patch 可被 `Patch.Invert`（patch.go:34）反转 |
| `MarshalFunc`/`UnmarshalFunc` option.go:43/53 | `marshalUnmarshal` compare.go:89 | 自定义 marshal 改变 normalize 结果。例：`UseNumber` 使数字以 `json.Number` 字符串比较（equal.go:88），`1` 与 `1.0` 从"相等"变为"replace"，操作集从空变为非空 |

**最短 patch 不是唯一正确答案**：同一对文档在不同选项下产生不同但都满足
round-trip 的操作集（例如 `[1,2,3]→[3,2,1]`：默认产生 3 个 replace，
`LCS()+Factorize()` 产生 2 个 move）。`Rationalize` 的成本模型
（operation.go:73 的字节估算）是启发式局部最优，不承诺全局最短。
验收标准是 `apply(diff(a,b),a)==b` 与顺序稳定性，而非操作数最少。

## 4. 顺序稳定性与已知非确定点

- 对象键遍历前排序（differ.go:246 `sortStrings`），patch 操作顺序对相同
  输入稳定。
- **已知例外**：`Factorize()` 下，源文档中存在多个相等的子树时，
  `prepare`（differ.go:156）按 Go map 随机遍历序写入哈希表，后写覆盖
  先写，导致 copy 的 `from` 在 `/a` 与 `/b` 之间随运行变化（实测 200 次
  运行出现 2 种结果）。两种 patch 语义等价、round-trip 均成立，但操作集
  不逐字节稳定。审计测试以"语义等价集合"断言，不把某一来源当唯一正确
  答案（`TestAudit_FactorizeCopySourceEquivalence`）。

## 5. 中途失败与状态恢复

- `CompareWithoutMarshal` 对非法类型经 recover 返回错误（compare.go:38-44），
  属于现有错误分层；同一输入重复调用得到相同错误，可重复。
- 复用 `Differ` 时，比较中途 panic（如深层非法类型）会在 `d.patch` 中留下
  已生成的部分操作（实测：先序键的 replace 已入列）。**恢复协议**：复用前
  必须调用 `Differ.Reset`（differ.go:47）清空 patch/pointer/hashmap；
  未 Reset 的重试会把残留操作带进新结果。`Compare`/`CompareJSON` 每次新建
  `Differ`，无此问题。回归：`TestAudit_RecoverableFailure`，断言失败重试后
  的成功结果与全新 Differ 的结果逐操作一致，不携带失败调用的缓存、计数
  或临时状态。

## 6. 无法判定的输入

- 非法 JSON 类型（非 `json.Unmarshal` 产出类型集合）：显式返回错误而非猜测
  （`CompareWithoutMarshal`，compare.go:32；`Compare` 路径由 normalize 阶段的
  marshal/unmarshal 错误提前拦截，compare.go:53-60）。
- 非法 JSON 文档：`Patch.apply(src, true)` 返回 `invalid source document`
  （apply.go:28）。
- 非法 JSON Pointer：`toDotPath` 返回解析错误（apply.go:147，pointer.go:96+）。
- 反转不支持的操作：copy → `ErrAmbiguousCopyOp`（patch.go:8），缺少前置
  test 的 remove/replace → `ErrNonReversible`（patch.go:7）。
